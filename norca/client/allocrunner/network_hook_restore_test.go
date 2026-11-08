// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/taskenv"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	"github.com/bacanoo-io/norca/norca/plugins/drivers/testutils"
)

// recordingConfigurator records the calls of the network hook.
type recordingConfigurator struct {
	calls  []string
	status *structs.AllocNetworkStatus
}

func (r *recordingConfigurator) Setup(context.Context, *structs.Allocation, *drivers.NetworkIsolationSpec) (*structs.AllocNetworkStatus, error) {
	r.calls = append(r.calls, "setup")
	return r.status, nil
}

func (r *recordingConfigurator) Teardown(context.Context, *structs.Allocation, *drivers.NetworkIsolationSpec) error {
	r.calls = append(r.calls, "teardown")
	return nil
}

type recordingStatusSetter struct{ status *structs.AllocNetworkStatus }

func (r *recordingStatusSetter) SetNetworkStatus(s *structs.AllocNetworkStatus) { r.status = s }

type noopIsolationSetter struct{}

func (noopIsolationSetter) SetNetworkIsolation(*drivers.NetworkIsolationSpec) {}

// restoredHook is the network hook of a bridge alloc whose network already
// exists (the pause container is running), as after a client restart.
func restoredHook(t *testing.T, configured bool) (*networkHook, *recordingConfigurator, *recordingStatusSetter) {
	alloc := mock.Alloc()
	alloc.Job.TaskGroups[0].Networks = []*structs.NetworkResource{{Mode: "bridge"}}
	spec := &drivers.NetworkIsolationSpec{
		Mode:   drivers.NetIsolationModeGroup,
		Path:   "/var/run/docker/netns/abc",
		Labels: map[string]string{dockerNetSpecLabelKey: "0123456789abcdef"},
	}
	nm := &testutils.MockDriver{MockNetworkManager: testutils.MockNetworkManager{
		CreateNetworkF: func(string, *drivers.NetworkCreateRequest) (*drivers.NetworkIsolationSpec, bool, error) {
			return spec, false, nil
		},
	}}
	conf := &recordingConfigurator{status: &structs.AllocNetworkStatus{InterfaceName: "eth0", Address: "172.26.64.9"}}
	status := &recordingStatusSetter{}
	envBuilder := taskenv.NewBuilder(mock.Node(), alloc, nil, alloc.Job.Region)
	hook := newNetworkHook(testlog.HCLogger(t), noopIsolationSetter{}, alloc, nm, conf, status, envBuilder.Build())
	hook.namespaceConfigured = func(path string) (bool, error) {
		must.Eq(t, spec.Path, path)
		return configured, nil
	}
	return hook, conf, status
}

// A restored network whose namespace lost its interfaces (dockerd restarted
// the pause container with a new namespace) is configured again, after the
// CNI state of the earlier namespace is released.
func TestNetworkHook_Prerun_RestoredEmptyNamespace(t *testing.T) {
	ci.Parallel(t)
	hook, conf, status := restoredHook(t, false)

	must.NoError(t, hook.Prerun())
	must.Eq(t, []string{"teardown", "setup"}, conf.calls)
	must.NotNil(t, status.status)
	must.Eq(t, "172.26.64.9", status.status.Address)
	must.NotNil(t, hook.spec.HostsConfig)
	must.Eq(t, "0123456789ab", hook.spec.HostsConfig.Hostname)
}

// A restored network whose namespace is still configured is left as it is.
func TestNetworkHook_Prerun_RestoredConfiguredNamespace(t *testing.T) {
	ci.Parallel(t)
	hook, conf, status := restoredHook(t, true)

	must.NoError(t, hook.Prerun())
	must.SliceEmpty(t, conf.calls)
	must.Nil(t, status.status)
}
