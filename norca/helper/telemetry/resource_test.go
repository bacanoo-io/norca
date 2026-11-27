// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"os"
	"testing"

	"github.com/shoenig/test/must"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

func attrs(res *resource.Resource) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range res.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

// Not parallel: it sets environment variables.
func TestNewResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "leak.key=1,service.name=other")
	t.Setenv("OTEL_SERVICE_NAME", "other")

	id := Identity{
		Version:    "0.3.0",
		HostName:   "host-a",
		Region:     "global",
		Datacenter: "dc1",
		NodeID:     "node-1",
		NodeName:   "client-a",
		NodePool:   "default",
		ServerID:   "server-1",
	}
	res, err := newResource(t.Context(), id, map[string]string{
		"deployment.environment.name": "prod",
		"host.name":                   "override",
	})
	must.NoError(t, err)
	got := attrs(res)

	for k, v := range map[string]string{
		"service.name":                "norca",
		"service.version":             "0.3.0",
		"service.instance.id":         "node-1",
		"host.name":                   "override",
		"norca.region":                "global",
		"norca.datacenter":            "dc1",
		"norca.node.id":               "node-1",
		"norca.node.name":             "client-a",
		"norca.node_pool":             "default",
		"norca.server.id":             "server-1",
		"deployment.environment.name": "prod",
	} {
		must.Eq(t, v, got[attribute.Key(k)].AsString(), must.Sprintf("attribute %s", k))
	}
	must.Eq(t, []string{"server", "client"}, got["norca.agent.roles"].AsStringSlice())
	must.Eq(t, int64(os.Getpid()), got["process.pid"].AsInt64())
	must.NotEq(t, "", got["os.type"].AsString())
	_, leaked := got["leak.key"]
	must.False(t, leaked, must.Sprint("OTEL_RESOURCE_ATTRIBUTES has no effect"))
}

func TestNewResource_ServerOnly(t *testing.T) {
	res, err := newResource(t.Context(), Identity{ServerID: "server-1"}, nil)
	must.NoError(t, err)
	got := attrs(res)
	must.Eq(t, "server-1", got["service.instance.id"].AsString())
	must.Eq(t, []string{"server"}, got["norca.agent.roles"].AsStringSlice())
	_, hasNode := got["norca.node.id"]
	must.False(t, hasNode)
}
