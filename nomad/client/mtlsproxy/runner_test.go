// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeServers answers the RPCs of the runner from memory, with blocking
// queries.
type fakeServers struct {
	lock       sync.Mutex
	index      uint64
	intentions []*structs.ServiceIntention
	regs       map[string][]*structs.ServiceRegistration // region/namespace/name
	roots      []*structs.MTLSRoot
	calls      map[string]int
	tokens     map[string]string // method -> last token
}

func newFakeServers() *fakeServers {
	return &fakeServers{index: 1, regs: make(map[string][]*structs.ServiceRegistration),
		calls: make(map[string]int), tokens: make(map[string]string)}
}

func (f *fakeServers) update(fn func()) {
	f.lock.Lock()
	defer f.lock.Unlock()
	fn()
	f.index++
}

func (f *fakeServers) wait(q *structs.QueryOptions) {
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		f.lock.Lock()
		index := f.index
		f.lock.Unlock()
		if q.MinQueryIndex == 0 || index > q.MinQueryIndex {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeServers) RPC(method string, args, reply any) error {
	switch method {
	case "ServiceIntention.List":
		a := args.(*structs.ServiceIntentionListRequest)
		f.wait(&a.QueryOptions)
		f.lock.Lock()
		defer f.lock.Unlock()
		f.calls[method]++
		f.tokens[method] = a.AuthToken
		r := reply.(*structs.ServiceIntentionListResponse)
		for _, i := range f.intentions {
			if i.Namespace == a.Namespace {
				r.Intentions = append(r.Intentions, i)
			}
		}
		r.Index = f.index
	case structs.ServiceRegistrationGetServiceRPCMethod:
		a := args.(*structs.ServiceRegistrationByNameRequest)
		f.wait(&a.QueryOptions)
		f.lock.Lock()
		defer f.lock.Unlock()
		f.calls[method]++
		f.tokens[method] = a.AuthToken
		r := reply.(*structs.ServiceRegistrationByNameResponse)
		r.Services = f.regs[a.Region+"/"+a.Namespace+"/"+a.ServiceName]
		r.Index = f.index
	case "MTLS.Roots":
		a := args.(*structs.MTLSRootsRequest)
		f.wait(&a.QueryOptions)
		f.lock.Lock()
		defer f.lock.Unlock()
		r := reply.(*structs.MTLSRootsResponse)
		r.Roots = f.roots
		r.Index = f.index
	case structs.ServiceRegistrationListRPCMethod:
		a := args.(*structs.ServiceRegistrationListRequest)
		f.wait(&a.QueryOptions)
		f.lock.Lock()
		defer f.lock.Unlock()
		f.calls[method]++
		r := reply.(*structs.ServiceRegistrationListResponse)
		byNS := make(map[string]*structs.ServiceRegistrationListStub)
		for key, regs := range f.regs {
			parts := strings.SplitN(key, "/", 3)
			if parts[0] != a.Region || len(regs) == 0 {
				continue
			}
			stub, ok := byNS[parts[1]]
			if !ok {
				stub = &structs.ServiceRegistrationListStub{Namespace: parts[1]}
				byNS[parts[1]] = stub
				r.Services = append(r.Services, stub)
			}
			stub.Services = append(stub.Services, &structs.ServiceRegistrationStub{ServiceName: parts[2]})
		}
		r.Index = f.index
	default:
		return fmt.Errorf("unexpected RPC %s", method)
	}
	return nil
}

func instance(address string, port int, mode string) *structs.ServiceRegistration {
	return &structs.ServiceRegistration{Address: address, Port: port, Ready: true,
		Meta: map[string]string{structs.MTLSServiceMetaKey: mode}}
}

// writeIdentity writes the files of the mtls hook.
func writeIdentity(t *testing.T, dir, cert string) {
	t.Helper()
	ca, _, _ := testCA(t, "global.nomad")
	must.NoError(t, os.MkdirAll(dir, 0o755))
	must.NoError(t, os.WriteFile(filepath.Join(dir, certFile), []byte(cert), 0o600))
	must.NoError(t, os.WriteFile(filepath.Join(dir, keyFile), []byte("key"), 0o600))
	must.NoError(t, os.WriteFile(filepath.Join(dir, bundleFile), ca, 0o600))
}

func waitForFile(t *testing.T, dir, name, substr string) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			b, err := os.ReadFile(filepath.Join(dir, name))
			return err == nil && strings.Contains(string(b), substr)
		}),
		wait.Timeout(5*time.Second),
		wait.Gap(10*time.Millisecond),
	), must.Sprintf("%s does not contain %q", name, substr))
}

func TestRunner_Sidecar(t *testing.T) {
	ci.Parallel(t)

	servers := newFakeServers()
	servers.roots = []*structs.MTLSRoot{
		{ID: "global-1", Region: "global", RootCert: "CA-GLOBAL-1"},
		{ID: "eu-1", Region: "eu", RootCert: "CA-EU-1"},
	}
	servers.intentions = []*structs.ServiceIntention{
		{Namespace: "default", SourceNamespace: "default", Source: "web", Destination: "api", Action: "allow"},
	}
	servers.regs["global/default/postgres"] = []*structs.ServiceRegistration{
		instance("10.0.0.1", 24000, structs.MTLSModeSidecar),
		instance("10.0.0.9", 5432, ""), // not an mtls instance
	}
	servers.regs["eu/default/"+structs.DefaultMTLSGatewayService] = []*structs.ServiceRegistration{
		instance("192.168.1.1", 8443, structs.MTLSModeGateway),
	}

	tmp := t.TempDir()
	certDir := filepath.Join(tmp, "mtls")
	confDir := filepath.Join(tmp, "envoy")
	must.NoError(t, os.MkdirAll(confDir, 0o755))
	writeIdentity(t, certDir, "CERT-1")

	r := NewRunner(RunnerConfig{
		Logger:       hclog.NewNullLogger(),
		RPC:          servers.RPC,
		CatalogToken: func() string { return "catalog-token" },
		NodeSecret:   func() string { return "node-secret" },
		Region:       "global",
		CertDir:      certDir,
		Writer:       &Writer{Dir: confDir},
		Interval:     20 * time.Millisecond,
		Sidecar: &Sidecar{
			NodeID: "n", Cluster: "api", ConfigDir: "/secrets/envoy",
			Namespace: "default", Service: "api", Region: "global",
			InboundPort: 25000, AppAddress: "127.0.0.1", AppPort: 8080,
			Upstreams: []Upstream{
				{Name: "db", LocalPort: 5432, Namespace: "default", Service: "postgres", Region: "global"},
				{Name: "remote", LocalPort: 6379, Namespace: "cache", Service: "redis", Region: "eu"},
			},
		},
	})
	must.NoError(t, r.Start(context.Background()))
	defer r.Stop()

	// the first configuration is complete when Start returns
	for _, name := range []string{BootstrapFile, ListenersFile, ClustersFile, CertSecretFile, TrustSecretFile,
		"eds-upstream_db.json", "eds-upstream_remote.json"} {
		_, err := os.Stat(filepath.Join(confDir, name))
		must.NoError(t, err, must.Sprintf("missing %s", name))
	}
	waitForFile(t, confDir, ListenersFile, "web.service.default.nomad")
	waitForFile(t, confDir, "eds-upstream_db.json", "10.0.0.1")
	waitForFile(t, confDir, "eds-upstream_remote.json", "192.168.1.1")
	waitForFile(t, confDir, ClustersFile, "redis.service.cache.eu.nomad")

	// each upstream trusts the CAs of the region of its destination only
	waitForFile(t, confDir, "sds-trust-upstream_db.json", "CA-GLOBAL-1")
	waitForFile(t, confDir, "sds-trust-upstream_remote.json", "CA-EU-1")
	dbTrust, err := os.ReadFile(filepath.Join(confDir, "sds-trust-upstream_db.json"))
	must.NoError(t, err)
	must.StrNotContains(t, string(dbTrust), "CA-EU")
	remoteTrust, err := os.ReadFile(filepath.Join(confDir, "sds-trust-upstream_remote.json"))
	must.NoError(t, err)
	must.StrNotContains(t, string(remoteTrust), "CA-GLOBAL")

	// a CA rotation in region eu (the retired root stays trusted) is followed
	servers.update(func() {
		servers.roots = append(servers.roots, &structs.MTLSRoot{ID: "eu-2", Region: "eu", RootCert: "CA-EU-2"})
	})
	waitForFile(t, confDir, "sds-trust-upstream_remote.json", "CA-EU-2")
	waitForFile(t, confDir, "sds-trust-upstream_remote.json", "CA-EU-1")
	db, err := os.ReadFile(filepath.Join(confDir, "eds-upstream_db.json"))
	must.NoError(t, err)
	must.StrNotContains(t, string(db), "10.0.0.9")

	// intentions, instances and certificates are followed
	servers.update(func() {
		servers.intentions = append(servers.intentions, &structs.ServiceIntention{
			Namespace: "default", SourceNamespace: "default", Source: "batch", Destination: "api", Action: "allow"})
		servers.regs["global/default/postgres"] = append(servers.regs["global/default/postgres"],
			instance("10.0.0.2", 24001, structs.MTLSModeApp))
	})
	waitForFile(t, confDir, ListenersFile, "batch.service.default.nomad")
	waitForFile(t, confDir, "eds-upstream_db.json", "10.0.0.2")

	writeIdentity(t, certDir, "CERT-2")
	waitForFile(t, confDir, CertSecretFile, "CERT-2")

	servers.lock.Lock()
	must.Eq(t, "node-secret", servers.tokens["ServiceIntention.List"])
	must.Eq(t, "catalog-token", servers.tokens[structs.ServiceRegistrationGetServiceRPCMethod])
	servers.lock.Unlock()
}

func TestRunner_SidecarWithoutCertificate(t *testing.T) {
	ci.Parallel(t)

	tmp := t.TempDir()
	r := NewRunner(RunnerConfig{
		Logger:       hclog.NewNullLogger(),
		RPC:          newFakeServers().RPC,
		CatalogToken: func() string { return "" },
		NodeSecret:   func() string { return "" },
		Region:       "global",
		CertDir:      filepath.Join(tmp, "missing"),
		Writer:       &Writer{Dir: tmp},
		Sidecar:      &Sidecar{Namespace: "default", Service: "api", Region: "global"},
	})
	must.ErrorContains(t, r.Start(context.Background()), "workload certificate")
}

func TestRunner_Gateway(t *testing.T) {
	ci.Parallel(t)

	servers := newFakeServers()
	servers.regs["eu/default/api"] = []*structs.ServiceRegistration{instance("10.1.0.1", 25001, structs.MTLSModeSidecar)}
	servers.regs["eu/cache/redis"] = []*structs.ServiceRegistration{instance("10.1.0.2", 6379, structs.MTLSModeApp)}
	servers.regs["eu/default/plain"] = []*structs.ServiceRegistration{instance("10.1.0.3", 80, "")}
	servers.regs["eu/default/mtls-gateway"] = []*structs.ServiceRegistration{instance("10.1.0.4", 8443, structs.MTLSModeGateway)}

	dir := t.TempDir()
	r := NewRunner(RunnerConfig{
		Logger:       hclog.NewNullLogger(),
		RPC:          servers.RPC,
		CatalogToken: func() string { return "" },
		NodeSecret:   func() string { return "" },
		Region:       "eu",
		Writer:       &Writer{Dir: dir},
		Interval:     20 * time.Millisecond,
		Gateway: &Gateway{NodeID: "n", Cluster: "mtls-gateway", ConfigDir: "/secrets/envoy", Region: "eu",
			ListenAddress: "0.0.0.0", ListenPort: 8443},
	})
	must.NoError(t, r.Start(context.Background()))
	defer r.Stop()

	waitForFile(t, dir, ListenersFile, "api.service.default.eu.nomad")
	waitForFile(t, dir, ListenersFile, "redis.service.cache.eu.nomad")
	waitForFile(t, dir, "eds-route_cache_redis.json", "10.1.0.2")
	lds, err := os.ReadFile(filepath.Join(dir, ListenersFile))
	must.NoError(t, err)
	must.StrNotContains(t, string(lds), "plain.service")
	must.StrNotContains(t, string(lds), "mtls-gateway.service")

	// a service that goes away loses its route and endpoints file
	servers.update(func() { delete(servers.regs, "eu/cache/redis") })
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			_, err := os.Stat(filepath.Join(dir, "eds-route_cache_redis.json"))
			return os.IsNotExist(err)
		}),
		wait.Timeout(5*time.Second),
		wait.Gap(10*time.Millisecond),
	))
	lds, err = os.ReadFile(filepath.Join(dir, ListenersFile))
	must.NoError(t, err)
	must.StrNotContains(t, string(lds), "redis.service")
}

func TestWriter(t *testing.T) {
	ci.Parallel(t)

	dir := t.TempDir()
	w := &Writer{Dir: dir}
	changed, err := w.Write(map[string][]byte{ListenersFile: []byte("l1"), "eds-a.json": []byte("a")})
	must.NoError(t, err)
	must.True(t, changed)

	changed, err = w.Write(map[string][]byte{ListenersFile: []byte("l1"), "eds-a.json": []byte("a")})
	must.NoError(t, err)
	must.False(t, changed)

	// a new writer (client restart) compares with the files on disk
	w = &Writer{Dir: dir}
	changed, err = w.Write(map[string][]byte{ListenersFile: []byte("l1"), "eds-a.json": []byte("a")})
	must.NoError(t, err)
	must.False(t, changed)

	must.NoError(t, os.WriteFile(filepath.Join(dir, "unrelated"), []byte("x"), 0o600))
	changed, err = w.Write(map[string][]byte{ListenersFile: []byte("l2")})
	must.NoError(t, err)
	must.True(t, changed)
	b, err := os.ReadFile(filepath.Join(dir, ListenersFile))
	must.NoError(t, err)
	must.Eq(t, "l2", string(b))
	_, err = os.Stat(filepath.Join(dir, "eds-a.json"))
	must.True(t, os.IsNotExist(err))
	_, err = os.Stat(filepath.Join(dir, "unrelated"))
	must.NoError(t, err)

	// no temporary files are left behind
	entries, err := os.ReadDir(dir)
	must.NoError(t, err)
	must.Len(t, 2, entries)
}
