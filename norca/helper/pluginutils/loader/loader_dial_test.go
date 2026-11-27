// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package loader

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/plugins/base"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc"
)

// Only the driver clients get the extra dial options (the trace
// instrumentation): no device call is made within a span.
func TestPluginLoader_DialOptions(t *testing.T) {
	ci.Parallel(t)

	opts := []grpc.DialOption{grpc.WithUserAgent("test")}
	l, err := NewPluginLoader(&PluginLoaderConfig{
		Logger:                testlog.HCLogger(t),
		PluginDir:             t.TempDir(),
		SupportedVersions:     AgentSupportedApiVersions,
		DriverGRPCDialOptions: opts,
	})
	must.NoError(t, err)
	must.Len(t, 1, l.dialOptions(base.PluginTypeDriver))
	must.Nil(t, l.dialOptions(base.PluginTypeDevice))
}
