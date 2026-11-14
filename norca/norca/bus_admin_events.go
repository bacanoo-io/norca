// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// The admin events of the system bus. The event stream API decides per
// topic who sees an event (stream.aclAllowsSubscription and the topic
// policies); the bus decides by subject only. Events that the API shows
// only with a right beyond reading the jobs of a namespace would reach
// every token whose bus rule covers <prefix>.<region>.>, e.g. the ACL auth
// methods with their OIDC client secret. These events go to their own
// stream, NORCA_ADMIN_EVENTS_<region>, on <prefix>.admin.<region>.<ns>.<topic>,
// and the authorizer withholds from a connection the admin topics its token
// may not see (busAdminDeny).

// busAdminTopics are the admin topics with the right the event stream API
// asks for them.
var busAdminTopics = []struct {
	topic   structs.Topic
	allowed func(*acl.ACL) bool
}{
	{structs.TopicACLToken, (*acl.ACL).IsManagement},
	{structs.TopicACLPolicy, (*acl.ACL).IsManagement},
	{structs.TopicACLRole, (*acl.ACL).IsManagement},
	{structs.TopicACLAuthMethod, (*acl.ACL).IsManagement},
	{structs.TopicACLBindingRule, (*acl.ACL).IsManagement},
	{structs.TopicNodePool, (*acl.ACL).IsManagement},
	{structs.TopicNode, (*acl.ACL).AllowNodeRead},
	{structs.TopicQuota, (*acl.ACL).AllowQuotaRead},
	{structs.TopicAdmissionPolicy, (*acl.ACL).AllowAdmissionRead},
	{structs.TopicSecretAccess, (*acl.ACL).AllowOperatorRead},
	{structs.TopicServer, (*acl.ACL).AllowOperatorRead},
	{structs.TopicBackup, (*acl.ACL).AllowBackupRead},
	// Only the admin network policies; those of a namespace stay in the
	// event stream (busAdminEvent).
	{structs.TopicNetworkPolicy, (*acl.ACL).AllowOperatorRead},
}

// busAdminEvent reports whether an event goes to the admin event stream.
func busAdminEvent(ev *structs.Event) bool {
	if ev.Topic == structs.TopicNetworkPolicy {
		_, admin := ev.Payload.(*structs.AdminNetworkPolicyEvent)
		return admin
	}
	for _, t := range busAdminTopics {
		if t.topic == ev.Topic {
			return true
		}
	}
	return false
}

// busAdminDeny returns what a connection whose token resolved to aclObj
// may not publish or receive, whatever its bus rule says:
//   - the admin topics its token may not see;
//   - publishing on the admin subjects (only the leader writes them);
//   - without a management token, the JetStream API of the admin event
//     stream (a consumer reads every topic of a stream, so the per-topic
//     rights can't hold there), and creating or changing streams (a stream
//     could source the admin stream; the bus is not a broker for
//     applications).
//
// A management token has every right and gets no deny list.
func (s *Server) busAdminDeny(aclObj *acl.ACL) (publish, subscribe []string) {
	if aclObj == nil || aclObj.IsManagement() {
		return nil, nil
	}
	prefix := s.config.Bus.Events.SubjectPrefix
	if prefix == "" {
		prefix = bus.DefaultSubjectPrefix
	}
	admin := prefix + "." + bus.AdminSubjectToken
	for _, t := range busAdminTopics {
		if !t.allowed(aclObj) {
			subscribe = append(subscribe, admin+".*.*."+string(t.topic))
		}
	}
	stream := bus.AdminEventStreamName(s.config.Region)
	publish = []string{
		admin + ".>",
		"$JS.API.STREAM.*." + stream,
		"$JS.API.STREAM.*.*." + stream,
		"$JS.API.CONSUMER.*." + stream,
		"$JS.API.CONSUMER.*." + stream + ".>",
		"$JS.API.CONSUMER.*.*." + stream,
		"$JS.API.CONSUMER.*.*." + stream + ".>",
		"$JS.API.DIRECT.GET." + stream,
		"$JS.API.DIRECT.GET." + stream + ".>",
		"$JS.ACK." + stream + ".>",
		"$JS.API.STREAM.CREATE.>",
		"$JS.API.STREAM.UPDATE.>",
	}
	return publish, subscribe
}
