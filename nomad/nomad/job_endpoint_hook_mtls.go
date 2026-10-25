// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
)

// jobMTLSHook validates jobs that use native mTLS: every server must run
// Norca, the certificate authority must be enabled and the namespace, job and
// group names must fit into a SPIFFE ID. It also injects the Envoy tasks of
// sidecar and gateway mode services.
type jobMTLSHook struct {
	srv *Server
}

func (jobMTLSHook) Name() string {
	return "mtls"
}

func (h jobMTLSHook) Validate(job *structs.Job) ([]error, error) {
	if !job.UsesMTLS() {
		return nil, nil
	}
	if !ServersMeetMinimumNorcaVersion(h.srv.serf.Members(), h.srv.Region(), false) {
		return nil, fmt.Errorf("all servers must be running Norca %v or later to use mtls blocks", minNorcaFeaturesVersion)
	}
	if !h.srv.mtlsConfig().IsEnabled() {
		return nil, fmt.Errorf("mtls blocks require native mTLS, which is disabled in the server configuration")
	}

	var mErr multierror.Error
	var warnings []error
	if err := structs.ValidateSPIFFESegment("namespace", job.Namespace); err != nil {
		mErr.Errors = append(mErr.Errors, err)
	}
	if job.ParentID == "" {
		if err := structs.ValidateSPIFFESegment("job ID", job.ID); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
	}
	for _, tg := range job.TaskGroups {
		services := tg.MTLSServices()
		if len(services) == 0 {
			continue
		}
		if err := structs.ValidateSPIFFESegment("group name", tg.Name); err != nil {
			mErr.Errors = append(mErr.Errors, err)
		}
		if err := tg.ValidateMTLSProxies(); err != nil {
			mErr.Errors = append(mErr.Errors, multierror.Prefix(err, fmt.Sprintf("group %q:", tg.Name)))
		}
		for _, service := range services {
			if !structs.IsValidMTLSServiceName(service.Name) {
				warnings = append(warnings, fmt.Errorf(
					"group %q: service %q is interpolated at runtime and is left out of the DNS names of the mTLS certificate, so intentions can't refer to it",
					tg.Name, service.Name))
			}
		}
	}
	return warnings, mErr.ErrorOrNil()
}

// Mutate injects an Envoy task for every sidecar and gateway mode service:
// a prestart sidecar task "mtls-proxy-<service>" with a dynamic port
// "mtls-proxy-<service>" for its inbound listener, or a main task
// "mtls-gateway-<service>" listening on the port of the gateway service. A
// task of the group with the TaskKind of the injected task
// ("mtls-proxy:<service>", "mtls-gateway:<service>") replaces it, so jobs can
// define the proxy task themselves. The client writes the Envoy configuration
// of these tasks.
func (jobMTLSHook) Mutate(job *structs.Job) (*structs.Job, []error, error) {
	for _, tg := range job.TaskGroups {
		// Validate reports groups with an unsupported network setup.
		if len(tg.Networks) != 1 {
			continue
		}
		injected := false
		for _, service := range tg.Services {
			if service == nil {
				continue
			}
			switch {
			case service.MTLS.IsSidecar():
				kind := structs.NewTaskKind(structs.MTLSProxyPrefix, service.Name)
				if !groupHasTaskKind(tg, kind) {
					task := newMTLSEnvoyTask(tg, kind, groupConnectGuessTaskDriver(tg), false)
					task.Lifecycle = &structs.TaskLifecycleConfig{
						Hook:    structs.TaskLifecycleHookPrestart,
						Sidecar: true,
					}
					tg.Tasks = append(tg.Tasks, task)
					task.Canonicalize(job, tg)
				}
				injectPort(tg, structs.MTLSProxyPortLabel(service.Name))
				injected = true

			case service.MTLS.IsGateway():
				kind := structs.NewTaskKind(structs.MTLSGatewayPrefix, service.Name)
				if !groupHasTaskKind(tg, kind) {
					task := newMTLSEnvoyTask(tg, kind, groupConnectGuessTaskDriver(tg), tg.Networks[0].Mode == "host")
					tg.Tasks = append(tg.Tasks, task)
					task.Canonicalize(job, tg)
				}
				injected = true
			}
		}
		if injected {
			// re-canonicalize the group network since this hook runs
			// after canonicalization
			tg.Networks[0].Canonicalize()
		}
	}
	return job, nil, nil
}

// groupHasTaskKind returns whether a task of the group has the kind.
func groupHasTaskKind(tg *structs.TaskGroup, kind structs.TaskKind) bool {
	for _, t := range tg.Tasks {
		if t.Kind == kind {
			return true
		}
	}
	return false
}

// newMTLSEnvoyTask returns the Envoy task of a sidecar or gateway mode
// service. The image and the log level come from node meta (with defaults
// applied by the client); the configuration is written by the client into
// the secrets directory of the task before it starts.
func newMTLSEnvoyTask(tg *structs.TaskGroup, kind structs.TaskKind, driver string, hostNetwork bool) *structs.Task {
	config := map[string]interface{}{
		"image": "${meta." + structs.MTLSEnvoyImageMeta + "}",
		"args": []interface{}{
			"-c", "${NOMAD_SECRETS_DIR}/envoy/bootstrap.json",
			"-l", "${meta." + structs.MTLSEnvoyLogLevelMeta + "}",
			"--disable-hot-restart",
		},
	}
	if hostNetwork {
		config["network_mode"] = "host"
	}

	// The name is used in container names, so it must start with
	// [A-Za-z0-9]; it gets a suffix if a task of the group has it already.
	name := kind.Name() + "-" + kind.Value()
	for _, t := range tg.Tasks {
		if t.Name == name {
			name = name + "-" + uuid.Generate()[:6]
			break
		}
	}
	return &structs.Task{
		Name:          name,
		Kind:          kind,
		Driver:        driver,
		Config:        config,
		ShutdownDelay: 5 * time.Second,
		LogConfig: &structs.LogConfig{
			MaxFiles:      2,
			MaxFileSizeMB: 2,
		},
		Resources: &structs.Resources{
			CPU:      100,
			MemoryMB: 128,
		},
		Constraints: structs.Constraints{{
			// only Norca clients write the Envoy configuration
			LTarget: "${attr.norca.version}",
			Operand: structs.ConstraintAttributeIsSet,
		}},
	}
}
