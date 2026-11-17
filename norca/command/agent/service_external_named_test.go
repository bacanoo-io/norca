// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A registration of a job's service named "external" is deleted like the one
// of any other service: with submit-job, without the capability of external
// services. DELETE /v1/service/external/:id is the path of both.
func TestHTTP_ServiceNamedExternal_Delete_ACL(t *testing.T) {
	ci.Parallel(t)

	httpACLTest(t, nil, func(s *TestAgent) {
		state := s.Agent.server.State()

		regs := mock.ServiceRegistrations()
		regs[0].ServiceName, regs[0].Namespace = "foo", structs.DefaultNamespace
		regs[1].ServiceName, regs[1].Namespace = "external", structs.DefaultNamespace
		must.NoError(t, state.UpsertServiceRegistrations(structs.MsgTypeTestSetup, 1000, regs))

		submit := mock.CreatePolicyAndToken(t, state, 1001, "submit",
			mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilitySubmitJob}))
		read := mock.CreatePolicyAndToken(t, state, 1003, "read",
			mock.NamespacePolicy(structs.DefaultNamespace, "", []string{acl.NamespaceCapabilityReadJob}))

		services := s.Client().Services()

		// without submit-job neither is deleted, and the answer is the same
		// whether the registration exists or not
		_, err := services.Delete("external", regs[1].ID, &api.WriteOptions{AuthToken: read.SecretID})
		must.ErrorContains(t, err, "Permission denied")
		_, err = services.Delete("external", "nope", &api.WriteOptions{AuthToken: read.SecretID})
		must.ErrorContains(t, err, "Permission denied")

		_, err = services.Delete("foo", regs[0].ID, &api.WriteOptions{AuthToken: submit.SecretID})
		must.NoError(t, err)
		_, err = services.Delete("external", regs[1].ID, &api.WriteOptions{AuthToken: submit.SecretID})
		must.NoError(t, err)

		out, err := state.GetServiceRegistrationByID(nil, structs.DefaultNamespace, regs[1].ID)
		must.NoError(t, err)
		must.Nil(t, out)
	})
}
