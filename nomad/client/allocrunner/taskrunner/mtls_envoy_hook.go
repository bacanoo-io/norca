// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/mtlsproxy"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// mtlsEnvoyDir is the directory of the Envoy configuration in the
	// secrets directory of an injected Envoy task.
	mtlsEnvoyDir = "envoy"

	// The defaults of the Envoy image of the injected tasks: the official
	// image, at a pinned release. Node meta (see structs.MTLSEnvoy*Meta)
	// overrides every part, so air-gapped clusters can use a mirror and
	// pin a digest.
	defaultEnvoyRegistry   = "docker.io"
	defaultEnvoyRepository = "envoyproxy/envoy"
	defaultEnvoyTag        = "v1.40.1"
	defaultEnvoyLogLevel   = "info"
)

// mtlsEnvoyHook configures the Envoy task injected for a native mTLS
// sidecar or gateway mode service: it resolves the image, writes the Envoy
// configuration into the secrets directory of the task and keeps it up to
// date (intentions, upstream instances, certificate renewals) while the task
// runs. The workload certificate of the task comes from the mtls hook, which
// runs before this hook.
type mtlsEnvoyHook struct {
	alloc        *structs.Allocation
	task         *structs.Task
	rpc          config.RPCer
	region       string
	nodeSecret   func() string
	catalogToken func() string
	logger       log.Logger

	lock   sync.Mutex
	runner *mtlsproxy.Runner
}

type mtlsEnvoyHookConfig struct {
	alloc        *structs.Allocation
	task         *structs.Task
	rpc          config.RPCer
	region       string
	nodeSecret   func() string
	catalogToken func() string
	logger       log.Logger
}

func newMTLSEnvoyHook(c *mtlsEnvoyHookConfig) *mtlsEnvoyHook {
	h := &mtlsEnvoyHook{
		alloc:        c.alloc,
		task:         c.task,
		rpc:          c.rpc,
		region:       c.region,
		nodeSecret:   c.nodeSecret,
		catalogToken: c.catalogToken,
	}
	h.logger = c.logger.Named(h.Name())
	return h
}

func (*mtlsEnvoyHook) Name() string {
	return "mtls_envoy"
}

func (h *mtlsEnvoyHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, _ *interfaces.TaskPrestartResponse) error {
	resolveMTLSEnvoyConfig(req.Task.Config, req.TaskEnv.NodeAttrs)

	h.lock.Lock()
	defer h.lock.Unlock()
	if h.runner != nil {
		// a restarted task: the configuration is kept up to date already
		return nil
	}

	dir := filepath.Join(req.TaskDir.SecretsDir, mtlsEnvoyDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create the Envoy configuration directory: %w", err)
	}
	inTask := filepath.ToSlash(filepath.Join(req.TaskEnv.EnvMap[taskenv.SecretsDir], mtlsEnvoyDir))

	cfg := mtlsproxy.RunnerConfig{
		Logger:       h.logger,
		RPC:          h.rpc.RPC,
		CatalogToken: h.catalogToken,
		NodeSecret:   h.nodeSecret,
		Region:       h.region,
		CertDir:      filepath.Join(req.TaskDir.SecretsDir, mtlsDir),
		Writer:       &mtlsproxy.Writer{Dir: dir, User: h.task.User},
	}
	if err := h.configure(&cfg, inTask); err != nil {
		return structs.NewRecoverableError(err, false)
	}

	runner := mtlsproxy.NewRunner(cfg)
	if err := runner.Start(ctx); err != nil {
		return structs.NewRecoverableError(fmt.Errorf("failed to write the Envoy configuration: %w", err), true)
	}
	h.runner = runner
	return nil
}

// configure fills in the sidecar or gateway configuration from the
// allocation.
func (h *mtlsEnvoyHook) configure(cfg *mtlsproxy.RunnerConfig, configDir string) error {
	tg := h.alloc.Job.LookupTaskGroup(h.alloc.TaskGroup)
	if tg == nil {
		return fmt.Errorf("task group %q not found", h.alloc.TaskGroup)
	}
	serviceName := h.task.Kind.Value()
	var service *structs.Service
	for _, s := range tg.Services {
		if s != nil && s.Name == serviceName && s.MTLS.NeedsProxy() {
			service = s
			break
		}
	}
	if service == nil {
		return fmt.Errorf("no sidecar or gateway mode mtls service %q in group %q", serviceName, tg.Name)
	}
	var ports structs.AllocatedPorts
	if h.alloc.AllocatedResources != nil {
		ports = h.alloc.AllocatedResources.Shared.Ports
	}
	nodeID := h.alloc.ID + "-" + h.task.Name

	if h.task.Kind.IsMTLSGateway() {
		mapping, ok := ports.Get(service.PortLabel)
		if !ok {
			return fmt.Errorf("port %q of gateway %q not allocated", service.PortLabel, service.Name)
		}
		port := mapping.Value
		if len(tg.Networks) == 1 && tg.Networks[0].Mode != "host" && mapping.To > 0 {
			port = mapping.To
		}
		cfg.Gateway = &mtlsproxy.Gateway{
			NodeID:        nodeID,
			Cluster:       service.Name,
			ConfigDir:     configDir,
			Region:        h.region,
			ListenAddress: "0.0.0.0",
			ListenPort:    port,
		}
		return nil
	}

	inbound, ok := ports.Get(structs.MTLSProxyPortLabel(service.Name))
	if !ok {
		return fmt.Errorf("port %q of the sidecar of %q not allocated", structs.MTLSProxyPortLabel(service.Name), service.Name)
	}
	appPort, err := mtlsAppPort(service.PortLabel, ports)
	if err != nil {
		return fmt.Errorf("service %q: %w", service.Name, err)
	}
	sidecar := &mtlsproxy.Sidecar{
		NodeID:      nodeID,
		Cluster:     service.Name,
		ConfigDir:   configDir,
		Namespace:   h.alloc.Namespace,
		Service:     service.Name,
		Region:      h.region,
		InboundPort: netnsPort(inbound),
		AppAddress:  "127.0.0.1",
		AppPort:     appPort,
	}
	cfg.Gateways = make(map[string]string)
	for _, u := range service.MTLS.Upstreams {
		if u == nil {
			continue
		}
		up := mtlsproxy.Upstream{
			Name:      u.Name,
			LocalPort: u.LocalPort,
			Namespace: u.Namespace,
			Service:   u.Service,
			Region:    u.Region,
		}
		if up.Namespace == "" {
			up.Namespace = h.alloc.Namespace
		}
		if up.Region == "" {
			up.Region = h.region
		}
		cfg.Gateways[u.Name] = u.GatewayService()
		sidecar.Upstreams = append(sidecar.Upstreams, up)
	}
	cfg.Sidecar = sidecar
	return nil
}

// mtlsCatalogToken returns the token the Envoy configuration reads the
// catalog with, like the built-in DNS server: the token of the client dns
// block (a token valid in every region is needed for upstreams of other
// regions when ACLs are enabled), or the identity of the node.
func (tr *TaskRunner) mtlsCatalogToken() string {
	if dns := tr.clientConfig.DNS; dns != nil && dns.Token != "" {
		return dns.Token
	}
	return tr.clientConfig.Node.SecretID
}

// netnsPort returns the port of a mapping inside the network namespace of
// the group.
func netnsPort(m structs.AllocatedPortMapping) int {
	if m.To > 0 {
		return m.To
	}
	return m.Value
}

// mtlsAppPort returns the port the application listens on inside the
// network namespace: a number, or the label of a port of the group network.
func mtlsAppPort(label string, ports structs.AllocatedPorts) (int, error) {
	if mapping, ok := ports.Get(label); ok {
		return netnsPort(mapping), nil
	}
	port, err := strconv.Atoi(label)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %q is neither a port label of the group nor a port number", label)
	}
	return port, nil
}

// Stop stops updating the configuration.
func (h *mtlsEnvoyHook) Stop(context.Context, *interfaces.TaskStopRequest, *interfaces.TaskStopResponse) error {
	h.stop()
	return nil
}

// Shutdown stops updating the configuration when the client shuts down; the
// task keeps running with its last configuration.
func (h *mtlsEnvoyHook) Shutdown() {
	h.stop()
}

func (h *mtlsEnvoyHook) stop() {
	h.lock.Lock()
	runner := h.runner
	h.runner = nil
	h.lock.Unlock()
	if runner != nil {
		runner.Stop()
	}
}

// mtlsEnvoyImage returns the Envoy image from node meta: the image meta if
// set, otherwise the image built from the registry, repository, tag and
// digest meta with their defaults.
func mtlsEnvoyImage(attrs map[string]string) string {
	meta := func(key, def string) string {
		if v, ok := attrs["meta."+key]; ok {
			return v
		}
		return def
	}
	if image := meta(structs.MTLSEnvoyImageMeta, ""); image != "" {
		return image
	}
	image := meta(structs.MTLSEnvoyRepositoryMeta, defaultEnvoyRepository)
	if registry := meta(structs.MTLSEnvoyRegistryMeta, defaultEnvoyRegistry); registry != "" {
		image = strings.TrimSuffix(registry, "/") + "/" + image
	}
	if tag := meta(structs.MTLSEnvoyTagMeta, defaultEnvoyTag); tag != "" {
		image += ":" + tag
	}
	if digest := meta(structs.MTLSEnvoyDigestMeta, ""); digest != "" {
		image += "@" + digest
	}
	return image
}

// resolveMTLSEnvoyConfig replaces the node meta references of the injected
// task configuration that have client-side defaults (the image and the log
// level), before the task configuration is interpolated.
func resolveMTLSEnvoyConfig(cfg map[string]interface{}, attrs map[string]string) {
	logLevel := defaultEnvoyLogLevel
	if v, ok := attrs["meta."+structs.MTLSEnvoyLogLevelMeta]; ok && v != "" {
		logLevel = v
	}
	replacer := strings.NewReplacer(
		"${meta."+structs.MTLSEnvoyImageMeta+"}", mtlsEnvoyImage(attrs),
		"${meta."+structs.MTLSEnvoyLogLevelMeta+"}", logLevel,
	)
	if image, ok := cfg["image"].(string); ok {
		cfg["image"] = replacer.Replace(image)
	}
	switch args := cfg["args"].(type) {
	case []interface{}:
		for i, a := range args {
			if s, ok := a.(string); ok {
				args[i] = replacer.Replace(s)
			}
		}
	case []string:
		for i, a := range args {
			args[i] = replacer.Replace(a)
		}
	}
}
