// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// A stack request that a server which is not the leader takes reaches the
// leader with its files: the leader answers for the files, and does not
// say that the request names neither files nor a source.
func TestRigger_StackRequestForwardedWithFiles(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup1 := TestServer(t, func(c *Config) { c.BootstrapExpect = 2 })
	t.Cleanup(cleanup1)

	s2, cleanup2 := TestServer(t, func(c *Config) { c.BootstrapExpect = 2 })
	t.Cleanup(cleanup2)

	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	follower := s1
	if s1.IsLeader() {
		follower = s2
	}

	for _, method := range []string{"Rigger.StackPlan", "Rigger.StackStatus", "Rigger.StackApply", "Rigger.StackDestroy"} {
		req := RiggerStackRequest{
			Files:        []byte("not an archive"),
			WriteRequest: structs.WriteRequest{Region: follower.config.Region, Namespace: structs.DefaultNamespace},
		}

		var resp RiggerOperationResponse

		err := follower.RPC(method, &req, &resp)
		must.Error(t, err, must.Sprint(method))
		must.StrNotContains(t, err.Error(), "either published", must.Sprint(method))
	}
}
