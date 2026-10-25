// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/mtlsproxy"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestMTLSEnvoyImage(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, "docker.io/envoyproxy/envoy:"+defaultEnvoyTag, mtlsEnvoyImage(nil))
	must.Eq(t, "mirror.local:5000/envoyproxy/envoy:v1.39.5@sha256:abc", mtlsEnvoyImage(map[string]string{
		"meta." + structs.MTLSEnvoyRegistryMeta: "mirror.local:5000/",
		"meta." + structs.MTLSEnvoyTagMeta:      "v1.39.5",
		"meta." + structs.MTLSEnvoyDigestMeta:   "sha256:abc",
	}))
	must.Eq(t, "envoy@sha256:abc", mtlsEnvoyImage(map[string]string{
		"meta." + structs.MTLSEnvoyRegistryMeta:   "",
		"meta." + structs.MTLSEnvoyRepositoryMeta: "envoy",
		"meta." + structs.MTLSEnvoyTagMeta:        "",
		"meta." + structs.MTLSEnvoyDigestMeta:     "sha256:abc",
	}))
	must.Eq(t, "custom/envoy:1", mtlsEnvoyImage(map[string]string{
		"meta." + structs.MTLSEnvoyImageMeta:    "custom/envoy:1",
		"meta." + structs.MTLSEnvoyRegistryMeta: "ignored",
	}))

	cfg := map[string]interface{}{
		"image": "${meta." + structs.MTLSEnvoyImageMeta + "}",
		"args":  []interface{}{"-l", "${meta." + structs.MTLSEnvoyLogLevelMeta + "}", "-c", "${NOMAD_SECRETS_DIR}/envoy/bootstrap.json"},
	}
	resolveMTLSEnvoyConfig(cfg, map[string]string{"meta." + structs.MTLSEnvoyTagMeta: "v9"})
	must.Eq(t, "docker.io/envoyproxy/envoy:v9", cfg["image"])
	must.Eq[any](t, []interface{}{"-l", "info", "-c", "${NOMAD_SECRETS_DIR}/envoy/bootstrap.json"}, cfg["args"])

	cfg["args"] = []string{"${meta." + structs.MTLSEnvoyLogLevelMeta + "}"}
	resolveMTLSEnvoyConfig(cfg, map[string]string{"meta." + structs.MTLSEnvoyLogLevelMeta: "debug"})
	must.Eq[any](t, []string{"debug"}, cfg["args"])
}

func TestMTLSAppPort(t *testing.T) {
	ci.Parallel(t)

	ports := structs.AllocatedPorts{{Label: "http", Value: 23000, To: 8080}, {Label: "dyn", Value: 23001, To: -1}}
	p, err := mtlsAppPort("http", ports)
	must.NoError(t, err)
	must.Eq(t, 8080, p)
	p, err = mtlsAppPort("dyn", ports)
	must.NoError(t, err)
	must.Eq(t, 23001, p)
	p, err = mtlsAppPort("9090", ports)
	must.NoError(t, err)
	must.Eq(t, 9090, p)
	_, err = mtlsAppPort("nope", ports)
	must.ErrorContains(t, err, "neither a port label")
}

// fakeMeshServer adds the intention and catalog RPCs to the fake CA.
type fakeMeshServer struct {
	*fakeMTLSServer
}

func (f *fakeMeshServer) RPC(method string, args, reply any) error {
	switch method {
	case "ServiceIntention.List":
		time.Sleep(10 * time.Millisecond)
		r := reply.(*structs.ServiceIntentionListResponse)
		r.Intentions = []*structs.ServiceIntention{{Namespace: "default", SourceNamespace: "default",
			Source: "web", Destination: "api", Action: "allow"}}
		r.Index = 10
		return nil
	case structs.ServiceRegistrationGetServiceRPCMethod:
		time.Sleep(10 * time.Millisecond)
		a := args.(*structs.ServiceRegistrationByNameRequest)
		r := reply.(*structs.ServiceRegistrationByNameResponse)
		if a.ServiceName == "postgres" {
			r.Services = []*structs.ServiceRegistration{{Address: "10.0.0.7", Port: 24000, Ready: true,
				Meta: map[string]string{structs.MTLSServiceMetaKey: structs.MTLSModeSidecar}}}
		}
		r.Index = 10
		return nil
	}
	return f.fakeMTLSServer.RPC(method, args, reply)
}

func TestMTLSEnvoyHook_Sidecar(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.Alloc()
	tg := alloc.Job.TaskGroups[0]
	tg.Networks = structs.Networks{{Mode: "bridge"}}
	tg.Services = []*structs.Service{{
		Name: "api", PortLabel: "8080", Provider: structs.ServiceProviderNomad,
		MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeSidecar, Upstreams: []*structs.ServiceMTLSUpstream{
			{Name: "db", Service: "postgres", LocalPort: 5432},
		}},
	}}
	proxy := &structs.Task{
		Name:   "mtls-proxy-api",
		Kind:   structs.NewTaskKind(structs.MTLSProxyPrefix, "api"),
		Driver: "docker",
		Config: map[string]interface{}{
			"image": "${meta." + structs.MTLSEnvoyImageMeta + "}",
			"args":  []interface{}{"-l", "${meta." + structs.MTLSEnvoyLogLevelMeta + "}"},
		},
	}
	tg.Tasks = append(tg.Tasks, proxy)
	alloc.AllocatedResources.Shared.Ports = structs.AllocatedPorts{
		{Label: structs.MTLSProxyPortLabel("api"), Value: 25000, To: -1, HostIP: "10.0.0.1"},
	}
	alloc.SignedIdentities = map[string]string{proxy.Name: "jwt"}

	rpc := &fakeMeshServer{newFakeMTLSServer(t, time.Hour)}
	secrets := t.TempDir()
	req := &interfaces.TaskPrestartRequest{
		Task:    proxy,
		TaskDir: &allocdir.TaskDir{SecretsDir: secrets},
		TaskEnv: &taskenv.TaskEnv{
			EnvMap:    map[string]string{taskenv.SecretsDir: "/secrets"},
			NodeAttrs: map[string]string{"meta." + structs.MTLSEnvoyTagMeta: "v1.2.3"},
		},
	}

	certHook := newMTLSHook(&mtlsHookConfig{alloc: alloc, task: proxy, rpc: rpc, region: "global",
		nodeKey: "node-secret", events: &mockEmitter{}, logger: testlog.HCLogger(t)})
	must.NoError(t, certHook.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	defer certHook.Stop(context.Background(), nil, nil)

	h := newMTLSEnvoyHook(&mtlsEnvoyHookConfig{
		alloc: alloc, task: proxy, rpc: rpc, region: "global",
		nodeSecret:   func() string { return "node-secret" },
		catalogToken: func() string { return "node-secret" },
		logger:       testlog.HCLogger(t),
	})
	must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	defer h.Stop(context.Background(), nil, nil)

	must.Eq(t, "docker.io/envoyproxy/envoy:v1.2.3", proxy.Config["image"])
	must.Eq[any](t, []interface{}{"-l", "info"}, proxy.Config["args"])

	dir := filepath.Join(secrets, mtlsEnvoyDir)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		must.NoError(t, err)
		var v any
		must.NoError(t, json.Unmarshal(b, &v))
		return string(b)
	}
	must.StrContains(t, read("bootstrap.json"), `"path": "/secrets/envoy/lds.json"`)
	lds := read("lds.json")
	must.StrContains(t, lds, `"port_value": 25000`)
	must.StrContains(t, lds, `"port_value": 5432`)
	must.StrContains(t, lds, "web.service.default.nomad")
	cds := read("cds.json")
	must.StrContains(t, cds, `"port_value": 8080`)
	must.StrContains(t, cds, "postgres.service.default.global.nomad")
	must.StrContains(t, read("eds-upstream_db.json"), "10.0.0.7")
	cert := read("sds-cert.json")
	must.StrContains(t, cert, "BEGIN CERTIFICATE")
	must.StrContains(t, cert, "PRIVATE KEY")
	must.True(t, strings.Contains(read("sds-trust.json"), `"name": "global.nomad"`))

	// a restart keeps the running configuration
	must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	must.NoError(t, h.Stop(context.Background(), nil, nil))
	h.lock.Lock()
	must.Nil(t, h.runner)
	h.lock.Unlock()
}

func TestMTLSEnvoyHook_Configure(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.Alloc()
	tg := alloc.Job.TaskGroups[0]
	tg.Networks = structs.Networks{{Mode: "bridge"}}
	tg.Services = []*structs.Service{{
		Name: "mtls-gateway", PortLabel: "gw", Provider: structs.ServiceProviderNomad,
		MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeGateway},
	}}
	alloc.AllocatedResources.Shared.Ports = structs.AllocatedPorts{{Label: "gw", Value: 8443, To: 9443}}
	task := &structs.Task{Name: "mtls-gateway-mtls-gateway", Kind: structs.NewTaskKind(structs.MTLSGatewayPrefix, "mtls-gateway")}

	h := newMTLSEnvoyHook(&mtlsEnvoyHookConfig{alloc: alloc, task: task, region: "eu", logger: testlog.HCLogger(t)})
	rc := &mtlsproxy.RunnerConfig{}
	must.NoError(t, h.configure(rc, "/secrets/envoy"))
	must.NotNil(t, rc.Gateway)
	must.Eq(t, 9443, rc.Gateway.ListenPort)
	must.Eq(t, "eu", rc.Gateway.Region)

	// host networking listens on the host port
	tg.Networks[0].Mode = "host"
	rc = &mtlsproxy.RunnerConfig{}
	must.NoError(t, h.configure(rc, "/secrets/envoy"))
	must.Eq(t, 8443, rc.Gateway.ListenPort)

	// the service must exist
	task.Kind = structs.NewTaskKind(structs.MTLSGatewayPrefix, "other")
	must.ErrorContains(t, h.configure(&mtlsproxy.RunnerConfig{}, "/secrets/envoy"), "no sidecar or gateway mode mtls service")
}
