// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// checked_by makes a client node of the caller's choosing run the http and
// tcp checks the caller wrote (review finding SRV-5): like binding the
// registration to a node by ID, it needs node write next to
// register-external-service.
func TestServiceRegistration_External_CheckedByACL(t *testing.T) {
	ci.Parallel(t)

	leader, root, cleanupLeader := TestACLServer(t, nil)
	t.Cleanup(cleanupLeader)
	testutil.WaitForLeader(t, leader.RPC)
	// The follower joins the leader, which stays the leader: the tokens and
	// the node are written into its state only, and it answers the calls
	// the follower forwards.
	follower, cleanupFollower := TestServer(t, func(c *Config) {
		c.BootstrapExpect = 0
		c.ACLEnabled = true
	})
	t.Cleanup(cleanupFollower)
	TestJoin(t, leader, follower)
	testutil.WaitForLeader(t, follower.RPC)
	must.True(t, leader.IsLeader())

	registerRules := mock.NamespacePolicy(structs.DefaultNamespace, "",
		[]string{acl.NamespaceCapabilityRegisterExternalService})
	register := mock.CreatePolicyAndToken(t, leader.State(), 1001, "register", registerRules)
	nodeReader := mock.CreatePolicyAndToken(t, leader.State(), 1003, "node-reader",
		registerRules+"\nnode { policy = \"read\" }\nnode_pool \"*\" { policy = \"write\" }\n")
	nodeWriter := mock.CreatePolicyAndToken(t, leader.State(), 1005, "node-writer",
		registerRules+"\nnode { policy = \"write\" }\n")
	node := checkedNode(t, leader, 1010, "default", "dc1")

	service := func(id string) *structs.ServiceRegistration {
		reg := declaredHTTPService(id, &structs.ExternalCheckedBy{NodePool: "default"})
		reg.External.Checks[0].HTTP = "http://169.254.169.254/latest/meta-data/"
		return reg
	}

	// On the leader and through a follower that forwards the call.
	for name, s := range map[string]*Server{"leader": leader, "follower": follower} {
		id := "probe-" + name
		for _, token := range []*structs.ACLToken{register, nodeReader} {
			_, err := upsertExternal(t, s, service(id), token.SecretID, "")
			must.ErrorContains(t, err, "requires node write", must.Sprintf("%s, token %q", name, token.Name))
		}
		reg, err := leader.State().GetServiceRegistrationByID(nil, structs.DefaultNamespace, id)
		must.NoError(t, err)
		must.Nil(t, reg)

		_, err = upsertExternal(t, s, service(id), nodeWriter.SecretID, "")
		must.NoError(t, err, must.Sprint(name))
		_, err = upsertExternal(t, s, service(id+"-root"), root.SecretID, "")
		must.NoError(t, err, must.Sprint(name))
		reg, err = leader.State().GetServiceRegistrationByID(nil, structs.DefaultNamespace, id)
		must.NoError(t, err)
		must.NotNil(t, reg)
		must.Eq(t, node.ID, reg.NodeID)
	}

	// Without checked_by the capability alone still registers.
	_, err := upsertExternal(t, leader, ttlService("db", time.Minute), register.SecretID, "")
	must.NoError(t, err)
}
