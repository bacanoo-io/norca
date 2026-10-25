// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package mtlsproxy generates the Envoy configuration of the native mTLS
// sidecar and gateway tasks.
//
// Envoy gets a static bootstrap that points its listeners (LDS) and clusters
// (CDS) at files in the configuration directory; the clusters of the
// upstreams get their endpoints (EDS) and the TLS contexts their certificate
// and trust bundle (SDS) from files as well. Envoy watches these files and
// applies a new version when a file is atomically replaced (renamed into
// place), so the client changes endpoints, intentions and certificates
// without restarting Envoy, and no xDS server or gRPC dependency is needed.
//
// The output is deterministic: equal inputs give byte-identical files, so
// the writer only replaces files whose content changed.
package mtlsproxy

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// The files of the configuration directory.
const (
	BootstrapFile   = "bootstrap.json"
	ListenersFile   = "lds.json"
	ClustersFile    = "cds.json"
	CertSecretFile  = "sds-cert.json"
	TrustSecretFile = "sds-trust.json"

	// endpointsPrefix starts the names of the EDS files, one per cluster.
	endpointsPrefix = "eds-"

	// upstreamTrustPrefix starts the names of the SDS files of the
	// validation contexts of upstream clusters.
	upstreamTrustPrefix = "sds-trust-"
)

// The names of secrets, clusters and listeners.
const (
	certSecretName  = "workload"
	trustSecretName = "trust"

	localAppCluster  = "local_app"
	inboundListener  = "inbound"
	gatewayListener  = "gateway"
	unknownCluster   = "gateway_unknown"
	upstreamPrefix   = "upstream_"
	gatewayRoutePref = "route_"

	// spiffeValidator is the name of the Envoy certificate validator that
	// verifies peers per SPIFFE trust domain.
	spiffeValidator = "envoy.tls.cert_validator.spiffe"
)

// The type URLs of the Envoy v3 API.
const (
	typeListener      = "type.googleapis.com/envoy.config.listener.v3.Listener"
	typeCluster       = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	typeLoadAssign    = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	typeSecret        = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret"
	typeTCPProxy      = "type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy"
	typeRBAC          = "type.googleapis.com/envoy.extensions.filters.network.rbac.v3.RBAC"
	typeDownstreamTLS = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext"
	typeUpstreamTLS   = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext"
	typeSPIFFE        = "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.SPIFFECertValidatorConfig"
	typeTLSInspector  = "type.googleapis.com/envoy.extensions.filters.listener.tls_inspector.v3.TlsInspector"
)

// connectTimeout is the timeout of connections to upstream hosts.
const connectTimeout = "5s"

// Endpoint is an address of an upstream host.
type Endpoint struct {
	Address string
	Port    int
}

// TrustBundle holds the CA certificates of one SPIFFE trust domain.
type TrustBundle struct {
	TrustDomain string
	PEM         string
}

// Identity is the workload certificate of the proxy, its key and the trust
// bundle, as written by the mtls hook.
type Identity struct {
	CertPEM []byte
	KeyPEM  []byte
	Bundles []TrustBundle
}

// ParseBundle groups the CA certificates of a PEM trust bundle by the trust
// domain of their spiffe://<trust-domain> URI SAN, which the CAs of every
// region carry. CAs without one are skipped.
func ParseBundle(bundle []byte) ([]TrustBundle, error) {
	byDomain := make(map[string]*strings.Builder)
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid certificate in the trust bundle: %w", err)
		}
		var domain string
		for _, u := range cert.URIs {
			if u.Scheme == "spiffe" && u.Host != "" && (u.Path == "" || u.Path == "/") {
				domain = u.Host
				break
			}
		}
		if domain == "" {
			continue
		}
		b, ok := byDomain[domain]
		if !ok {
			b = &strings.Builder{}
			byDomain[domain] = b
		}
		b.Write(pem.EncodeToMemory(block))
	}
	if len(byDomain) == 0 {
		return nil, errors.New("the trust bundle has no SPIFFE certificate authority")
	}
	out := make([]TrustBundle, 0, len(byDomain))
	for domain, b := range byDomain {
		out = append(out, TrustBundle{TrustDomain: domain, PEM: b.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TrustDomain < out[j].TrustDomain })
	return out, nil
}

// ServiceName returns the regional DNS name of a service written into the
// certificates of its workloads, lowercase: the SNI name of connections to
// the service and the name upstream connections verify.
func ServiceName(service, namespace, region string) string {
	return strings.ToLower(fmt.Sprintf("%s.service.%s.%s.%s", service, namespace, region, structs.DefaultMTLSDNSDomain))
}

// Upstream is an upstream of a sidecar.
type Upstream struct {
	// Name is the label of the upstream block.
	Name string

	// LocalPort is the port of the listener on 127.0.0.1.
	LocalPort int

	// Namespace, Service and Region identify the destination service.
	Namespace string
	Service   string
	Region    string

	// Endpoints are the instances of the service, or of the gateway of its
	// region for a service of another region.
	Endpoints []Endpoint

	// TrustedCAs are the PEM encoded CA certificates of the region of the
	// service (its current and retired roots): the only CAs the server
	// certificate may be signed by. Without them the upstream fails closed:
	// it gets no endpoints.
	TrustedCAs string
}

// Sidecar is the configuration of the sidecar of a service.
type Sidecar struct {
	// NodeID and Cluster identify the Envoy instance.
	NodeID  string
	Cluster string

	// ConfigDir is the configuration directory as seen by Envoy.
	ConfigDir string

	// Namespace, Service and Region of the service the sidecar fronts.
	Namespace string
	Service   string
	Region    string

	// InboundPort is the port of the mTLS listener, on every address of the
	// network namespace of the group.
	InboundPort int

	// AppAddress and AppPort are the address of the application.
	AppAddress string
	AppPort    int

	// Intentions are the service intentions of the namespace.
	Intentions []*structs.ServiceIntention

	// Upstreams are the upstreams of the service.
	Upstreams []Upstream
}

// Route is a destination of a gateway.
type Route struct {
	Namespace string
	Service   string
	Endpoints []Endpoint
}

// Gateway is the configuration of a cross-region gateway.
type Gateway struct {
	NodeID    string
	Cluster   string
	ConfigDir string

	// Region of the gateway; routes match the SNI names of this region.
	Region string

	// ListenAddress and ListenPort are the address of the listener.
	ListenAddress string
	ListenPort    int

	// Routes are the mTLS services of the region.
	Routes []Route
}

// Files returns the configuration files of the sidecar.
func (s *Sidecar) Files(id *Identity) (map[string][]byte, error) {
	if id == nil || len(id.CertPEM) == 0 || len(id.KeyPEM) == 0 || len(id.Bundles) == 0 {
		return nil, errors.New("the workload certificate is not available")
	}
	files := make(map[string][]byte)
	var err error
	add := func(name string, value any) {
		if err != nil {
			return
		}
		var b []byte
		b, err = marshal(value)
		files[name] = b
	}

	add(BootstrapFile, bootstrap(s.NodeID, s.Cluster, s.ConfigDir))
	add(CertSecretFile, discoveryResponse(certSecret(id)))
	add(TrustSecretFile, discoveryResponse(trustSecret(id.Bundles)))

	listeners := []any{s.inbound()}
	clusters := []any{staticCluster(localAppCluster, []Endpoint{{Address: s.AppAddress, Port: s.AppPort}})}
	upstreams := append([]Upstream(nil), s.Upstreams...)
	sort.Slice(upstreams, func(i, j int) bool { return upstreams[i].Name < upstreams[j].Name })
	for _, u := range upstreams {
		name := upstreamPrefix + u.Name
		listeners = append(listeners, listener(name, "127.0.0.1", u.LocalPort, nil,
			[]any{map[string]any{"filters": []any{tcpProxy(name, name)}}}, nil))

		sni := ServiceName(u.Service, u.Namespace, u.Region)
		clusters = append(clusters, s.edsCluster(name, upstreamTLS(s.ConfigDir, name, sni)))

		// Fail closed while the CAs of the region of the upstream are
		// unknown: no endpoints, and a validation context that can't
		// accept any server.
		endpoints, cas := u.Endpoints, u.TrustedCAs
		if strings.TrimSpace(cas) == "" {
			endpoints, cas, sni = nil, string(id.CertPEM), unmatchableName
		}
		add(endpointsFile(name), discoveryResponse(loadAssignment(typeLoadAssign, name, endpoints)))
		add(upstreamTrustFile(name), discoveryResponse(upstreamTrustSecret(name, cas, sni)))
	}
	add(ListenersFile, discoveryResponse(listeners...))
	add(ClustersFile, discoveryResponse(clusters...))
	return files, err
}

// inbound returns the mTLS listener in front of the application.
func (s *Sidecar) inbound() any {
	rbac := map[string]any{
		"@type":       typeRBAC,
		"stat_prefix": "inbound_rbac",
		"rules": map[string]any{
			"action":   "ALLOW",
			"policies": RBACPolicies(s.Intentions, s.Namespace, s.Service),
		},
	}
	chain := map[string]any{
		"transport_socket": transportSocket(map[string]any{
			"@type":                      typeDownstreamTLS,
			"require_client_certificate": true,
			"common_tls_context":         commonTLS(s.ConfigDir, trustSecretName, TrustSecretFile),
		}),
		"filters": []any{
			map[string]any{"name": "envoy.filters.network.rbac", "typed_config": rbac},
			tcpProxy(inboundListener, localAppCluster),
		},
	}
	return listener(inboundListener, "0.0.0.0", s.InboundPort, nil, []any{chain}, nil)
}

func (s *Sidecar) edsCluster(name string, tls any) any {
	return edsCluster(s.ConfigDir, name, tls)
}

// Files returns the configuration files of the gateway.
func (g *Gateway) Files() (map[string][]byte, error) {
	files := make(map[string][]byte)
	var err error
	add := func(name string, value any) {
		if err != nil {
			return
		}
		var b []byte
		b, err = marshal(value)
		files[name] = b
	}

	add(BootstrapFile, bootstrap(g.NodeID, g.Cluster, g.ConfigDir))

	routes := append([]Route(nil), g.Routes...)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Namespace != routes[j].Namespace {
			return routes[i].Namespace < routes[j].Namespace
		}
		return routes[i].Service < routes[j].Service
	})
	chains := []any{}
	clusters := []any{staticCluster(unknownCluster, nil)}
	seen := make(map[string]bool, len(routes))
	for _, r := range routes {
		sni := ServiceName(r.Service, r.Namespace, g.Region)
		if seen[sni] {
			continue
		}
		seen[sni] = true
		name := gatewayRoutePref + strings.ToLower(r.Namespace) + "_" + strings.ToLower(r.Service)
		chains = append(chains, map[string]any{
			"filter_chain_match": map[string]any{"server_names": []string{sni}},
			"filters":            []any{tcpProxy(name, name)},
		})
		clusters = append(clusters, edsCluster(g.ConfigDir, name, nil))
		add(endpointsFile(name), discoveryResponse(loadAssignment(typeLoadAssign, name, r.Endpoints)))
	}

	// TLS passthrough: the TLS inspector reads the SNI name, connections
	// without a known name go to a cluster without hosts and are closed.
	listenerFilters := []any{map[string]any{
		"name":         "envoy.filters.listener.tls_inspector",
		"typed_config": map[string]any{"@type": typeTLSInspector},
	}}
	defaultChain := map[string]any{"filters": []any{tcpProxy(unknownCluster, unknownCluster)}}
	add(ListenersFile, discoveryResponse(listener(gatewayListener, g.ListenAddress, g.ListenPort, listenerFilters, chains, defaultChain)))
	add(ClustersFile, discoveryResponse(clusters...))
	return files, err
}

// endpointsFile returns the EDS file of a cluster.
func endpointsFile(cluster string) string {
	return endpointsPrefix + cluster + ".json"
}

// IsEndpointsFile returns whether a file name is an EDS file, or the SDS
// file of an upstream cluster: the files that belong to one cluster.
func IsEndpointsFile(name string) bool {
	return (strings.HasPrefix(name, endpointsPrefix) || strings.HasPrefix(name, upstreamTrustPrefix)) &&
		strings.HasSuffix(name, ".json")
}

func bootstrap(nodeID, cluster, dir string) any {
	return map[string]any{
		"node": map[string]any{"id": nodeID, "cluster": cluster},
		"dynamic_resources": map[string]any{
			"lds_config": fileSource(dir, ListenersFile),
			"cds_config": fileSource(dir, ClustersFile),
		},
	}
}

// fileSource returns a config source reading a file of the configuration
// directory. Envoy reloads the file when it is moved into place.
func fileSource(dir, file string) any {
	return map[string]any{
		"path_config_source":   map[string]any{"path": path.Join(dir, file)},
		"resource_api_version": "V3",
	}
}

// discoveryResponse wraps resources into the DiscoveryResponse a file source holds;
// its version is the hash of the resources.
func discoveryResponse(resources ...any) any {
	b, _ := json.Marshal(resources)
	sum := sha256.Sum256(b)
	return map[string]any{
		"version_info": hex.EncodeToString(sum[:8]),
		"resources":    resources,
	}
}

func certSecret(id *Identity) any {
	return map[string]any{
		"@type": typeSecret,
		"name":  certSecretName,
		"tls_certificate": map[string]any{
			"certificate_chain": map[string]any{"inline_string": string(id.CertPEM)},
			"private_key":       map[string]any{"inline_string": string(id.KeyPEM)},
		},
	}
}

// trustSecret returns the validation context of the inbound listener: the
// SPIFFE validator verifies a client against the CAs of the trust domain of
// its SPIFFE ID only.
func trustSecret(bundles []TrustBundle) any {
	domains := make([]any, 0, len(bundles))
	for _, b := range bundles {
		domains = append(domains, map[string]any{
			"name":         b.TrustDomain,
			"trust_bundle": map[string]any{"inline_string": b.PEM},
		})
	}
	return map[string]any{
		"@type": typeSecret,
		"name":  trustSecretName,
		"validation_context": map[string]any{
			"custom_validator_config": map[string]any{
				"name": spiffeValidator,
				"typed_config": map[string]any{
					"@type":         typeSPIFFE,
					"trust_domains": domains,
				},
			},
		},
	}
}

// upstreamTrustSecret returns the validation context of an upstream
// cluster: the server certificate must be signed by a CA of the region of
// the destination (cas) and carry the regional DNS name of the service
// (san).
//
// It uses the default validator: the SPIFFE validator of Envoy ignores DNS
// subject alt name matchers (checked with Envoy 1.39.3 and 1.40.1: a server
// presenting the certificate of another service was accepted). Each
// upstream has its own secret, because the CAs and the name differ.
func upstreamTrustSecret(cluster, cas, san string) any {
	return map[string]any{
		"@type": typeSecret,
		"name":  upstreamTrustName(cluster),
		"validation_context": map[string]any{
			"trusted_ca": map[string]any{"inline_string": cas},
			"match_typed_subject_alt_names": []any{map[string]any{
				"san_type": "DNS",
				"matcher":  map[string]any{"exact": san, "ignore_case": true},
			}},
		},
	}
}

// unmatchableName is the DNS name an upstream requires while it fails
// closed; no workload certificate carries it.
const unmatchableName = "no-trusted-ca.invalid"

// upstreamTrustName and upstreamTrustFile name the validation context of
// an upstream cluster and its SDS file.
func upstreamTrustName(cluster string) string {
	return trustSecretName + "_" + cluster
}

func upstreamTrustFile(cluster string) string {
	return upstreamTrustPrefix + cluster + ".json"
}

func sdsConfig(dir, name, file string) any {
	return map[string]any{"name": name, "sds_config": fileSource(dir, file)}
}

// commonTLS returns the TLS context with the workload certificate and the
// validation context of the SDS secret trust in file trustFile.
func commonTLS(dir, trust, trustFile string) any {
	return map[string]any{
		"tls_params":                           map[string]any{"tls_minimum_protocol_version": "TLSv1_2"},
		"tls_certificate_sds_secret_configs":   []any{sdsConfig(dir, certSecretName, CertSecretFile)},
		"validation_context_sds_secret_config": sdsConfig(dir, trust, trustFile),
	}
}

// upstreamTLS originates mTLS to a service: the SNI name lets gateways
// route the connection, and the server certificate must carry the regional
// DNS name of the service (checked by the validation context of the
// cluster).
func upstreamTLS(dir, cluster, name string) any {
	return transportSocket(map[string]any{
		"@type":              typeUpstreamTLS,
		"sni":                name,
		"common_tls_context": commonTLS(dir, upstreamTrustName(cluster), upstreamTrustFile(cluster)),
	})
}

func transportSocket(config any) any {
	return map[string]any{"name": "envoy.transport_sockets.tls", "typed_config": config}
}

func tcpProxy(statPrefix, cluster string) any {
	return map[string]any{
		"name": "envoy.filters.network.tcp_proxy",
		"typed_config": map[string]any{
			"@type":       typeTCPProxy,
			"stat_prefix": statPrefix,
			"cluster":     cluster,
		},
	}
}

func listener(name, address string, port int, listenerFilters, chains []any, defaultChain any) any {
	l := map[string]any{
		"@type":         typeListener,
		"name":          name,
		"address":       socketAddress(address, port),
		"filter_chains": chains,
	}
	if len(listenerFilters) > 0 {
		l["listener_filters"] = listenerFilters
	}
	if defaultChain != nil {
		l["default_filter_chain"] = defaultChain
	}
	return l
}

func socketAddress(address string, port int) any {
	return map[string]any{"socket_address": map[string]any{"address": address, "port_value": port}}
}

func staticCluster(name string, endpoints []Endpoint) any {
	return map[string]any{
		"@type":           typeCluster,
		"name":            name,
		"type":            "STATIC",
		"connect_timeout": connectTimeout,
		"load_assignment": loadAssignment("", name, endpoints),
	}
}

func edsCluster(dir, name string, tls any) any {
	c := map[string]any{
		"@type":           typeCluster,
		"name":            name,
		"type":            "EDS",
		"connect_timeout": connectTimeout,
		"eds_cluster_config": map[string]any{
			"service_name": name,
			"eds_config":   fileSource(dir, endpointsFile(name)),
		},
	}
	if tls != nil {
		c["transport_socket"] = tls
	}
	return c
}

// loadAssignment returns the endpoints of a cluster, sorted; typeURL is set
// for EDS resources.
func loadAssignment(typeURL, cluster string, endpoints []Endpoint) any {
	sorted := append([]Endpoint(nil), endpoints...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Address != sorted[j].Address {
			return sorted[i].Address < sorted[j].Address
		}
		return sorted[i].Port < sorted[j].Port
	})
	lbEndpoints := make([]any, 0, len(sorted))
	var last Endpoint
	for i, e := range sorted {
		if i > 0 && e == last {
			continue
		}
		last = e
		lbEndpoints = append(lbEndpoints, map[string]any{
			"endpoint": map[string]any{"address": socketAddress(e.Address, e.Port)},
		})
	}
	la := map[string]any{"cluster_name": cluster}
	if len(lbEndpoints) > 0 {
		la["endpoints"] = []any{map[string]any{"lb_endpoints": lbEndpoints}}
	}
	if typeURL != "" {
		la["@type"] = typeURL
	}
	return la
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
