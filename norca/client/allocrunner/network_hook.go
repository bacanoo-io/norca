// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"
	"fmt"

	"github.com/bacanoo-io/norca/norca/client/taskenv"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/miekg/dns"
)

const (
	// dockerNetSpecLabelKey is the label added when we create a pause
	// container to own the network namespace, and the NetworkIsolationSpec we
	// get back from CreateNetwork has this label set as the container ID.
	// We'll use this to generate a hostname for the task in the event the user
	// did not specify a custom one. Please see dockerNetSpecHostnameKey.
	dockerNetSpecLabelKey = "docker_sandbox_container_id"

	// dockerNetSpecHostnameKey is the label added when we create a pause
	// container and the task group network include a user supplied hostname
	// parameter.
	dockerNetSpecHostnameKey = "docker_sandbox_hostname"
)

type networkIsolationSetter interface {
	SetNetworkIsolation(*drivers.NetworkIsolationSpec)
}

// allocNetworkIsolationSetter is a shim to allow the alloc network hook to
// set the alloc network isolation configuration without full access
// to the alloc runner
type allocNetworkIsolationSetter struct {
	ar *allocRunner
}

func (a *allocNetworkIsolationSetter) SetNetworkIsolation(n *drivers.NetworkIsolationSpec) {
	for _, tr := range a.ar.tasks {
		tr.SetNetworkIsolation(n)
	}
}

type networkStatusSetter interface {
	SetNetworkStatus(*structs.AllocNetworkStatus)
}

// networkHook is an alloc lifecycle hook that manages the network namespace
// for an alloc
type networkHook struct {
	// isolationSetter is a callback to set the network isolation spec when after the
	// network is created
	isolationSetter networkIsolationSetter

	// statusSetter is a callback to the alloc runner to set the network status once
	// network setup is complete
	networkStatusSetter networkStatusSetter

	// manager is used when creating the network namespace. This defaults to
	// bind mounting a network namespace descritor under /var/run/netns but
	// can be created by a driver if nessicary
	manager drivers.DriverNetworkManager

	// alloc should only be read from
	alloc *structs.Allocation

	// spec described the network namespace and is syncronized by specLock
	spec *drivers.NetworkIsolationSpec

	// networkConfigurator configures the network interfaces, routes, etc once
	// the alloc network has been created
	networkConfigurator NetworkConfigurator

	// taskEnv is used to perform interpolation within the network blocks.
	taskEnv *taskenv.TaskEnv

	// namespaceConfigured reports whether the network namespace at a path
	// has an interface besides the loopback one (namespaceConfigured of the
	// platform; replaced in tests).
	namespaceConfigured func(path string) (bool, error)

	logger hclog.Logger
}

func newNetworkHook(logger hclog.Logger,
	ns networkIsolationSetter,
	alloc *structs.Allocation,
	netManager drivers.DriverNetworkManager,
	netConfigurator NetworkConfigurator,
	networkStatusSetter networkStatusSetter,
	taskEnv *taskenv.TaskEnv,
) *networkHook {
	return &networkHook{
		isolationSetter:     ns,
		networkStatusSetter: networkStatusSetter,
		alloc:               alloc,
		manager:             netManager,
		networkConfigurator: netConfigurator,
		taskEnv:             taskEnv,
		namespaceConfigured: namespaceConfigured,
		logger:              logger,
	}
}

func (h *networkHook) Name() string {
	return "network"
}

func (h *networkHook) Prerun() error {
	tg := h.alloc.Job.LookupTaskGroup(h.alloc.TaskGroup)
	if len(tg.Networks) == 0 || tg.Networks[0].Mode == "host" || tg.Networks[0].Mode == "" {
		return nil
	}

	if h.manager == nil || h.networkConfigurator == nil {
		h.logger.Trace("shared network namespaces are not supported on this platform, skipping network hook")
		return nil
	}

	// Perform our networks block interpolation.
	interpolatedNetworks := taskenv.InterpolateNetworks(h.taskEnv, tg.Networks)

	// Interpolated values need to be validated. It is also possible a user
	// supplied hostname avoids the validation on job registrations because it
	// looks like it includes interpolation, when it doesn't.
	if interpolatedNetworks[0].Hostname != "" {
		if _, ok := dns.IsDomainName(interpolatedNetworks[0].Hostname); !ok {
			return fmt.Errorf("network hostname %q is not a valid DNS name", interpolatedNetworks[0].Hostname)
		}
	}

	// Our network create request.
	networkCreateReq := drivers.NetworkCreateRequest{
		Hostname: interpolatedNetworks[0].Hostname,
	}

	spec, created, err := h.manager.CreateNetwork(h.alloc.ID, &networkCreateReq)
	if err != nil {
		return fmt.Errorf("failed to create network for alloc: %v", err)
	}

	if spec != nil {
		h.spec = spec
		h.isolationSetter.SetNetworkIsolation(spec)
	}

	// clean room bugfix: a network that already existed (the restore after a
	// client restart) was taken as configured. When the machine restarted,
	// dockerd restarts the pause container (restart policy unless-stopped)
	// with a new, empty namespace: the tasks ran without an address and the
	// checks of their ports were refused. A namespace without any interface
	// but the loopback one is configured again, after the CNI state of its
	// earlier incarnation (address reservation, port mappings) is released.
	//
	// A network of the mode "none" has nothing but the loopback interface
	// by design, and nothing configures it: it is left alone.
	isConfigurable := tg.Networks[0].Mode != "none"
	if !created && isConfigurable && spec != nil && spec.Path != "" {
		configured, err := h.namespaceConfigured(spec.Path)
		if err != nil {
			h.logger.Warn("failed to check the network namespace of the alloc", "path", spec.Path, "error", err)
		} else if !configured {
			h.logger.Info("network namespace of the alloc has no interface, configuring it again", "path", spec.Path)
			if err := h.networkConfigurator.Teardown(context.TODO(), h.alloc, spec); err != nil {
				h.logger.Debug("releasing the earlier network configuration failed", "error", err)
			}
			created = true
		}
	}

	if created {
		status, err := h.networkConfigurator.Setup(context.TODO(), h.alloc, spec)
		if err != nil {
			return fmt.Errorf("failed to configure networking for alloc: %v", err)
		}

		// clean room bugfix: a network that nothing configures (the mode
		// "none": the configurator of the host network, which returns no
		// status) has no address for the hosts file of the tasks. With the
		// label of a Docker sandbox on its spec the status was read all the
		// same, and the nil status made the client panic.
		if status == nil {
			h.networkStatusSetter.SetNetworkStatus(nil)
			return nil
		}

		// If the driver set the sandbox hostname label, then we will use that
		// to set the HostsConfig.Hostname. Otherwise, identify the sandbox
		// container ID which will have been used to set the network namespace
		// hostname.
		if hostname, ok := spec.Labels[dockerNetSpecHostnameKey]; ok {
			h.spec.HostsConfig = &drivers.HostsConfig{
				Address:  status.Address,
				Hostname: hostname,
			}
		} else if hostname, ok := spec.Labels[dockerNetSpecLabelKey]; ok {

			// the docker_sandbox_container_id is the full ID of the pause
			// container, whereas we want the shortened name that dockerd sets
			// as the pause container's hostname.
			if len(hostname) > 12 {
				hostname = hostname[:12]
			}

			h.spec.HostsConfig = &drivers.HostsConfig{
				Address:  status.Address,
				Hostname: hostname,
			}
		}

		h.networkStatusSetter.SetNetworkStatus(status)
	}
	return nil
}

func (h *networkHook) Postrun() error {

	// we need the spec for network teardown
	if h.spec != nil {
		if err := h.networkConfigurator.Teardown(context.TODO(), h.alloc, h.spec); err != nil {
			h.logger.Error("failed to cleanup network for allocation, resources may have leaked", "alloc", h.alloc.ID, "error", err)
		}
	}

	// issue driver destroy regardless if we have a spec (e.g. cleanup pause container)
	return h.manager.DestroyNetwork(h.alloc.ID, h.spec)
}
