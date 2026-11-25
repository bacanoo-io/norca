// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/nats-io/nats.go/jetstream"
)

// Change data capture (CDC) on the system bus: a reader receives the change
// streams of the region (events, admin events, Consul KV notices) by the cdc
// rule of its policies, live and through the one consumer per stream the
// servers keep for it (/v1/cdc/consumer). The bus rule keeps the subjects
// outside the change streams and outside JetStream.

// busEventsPrefix is the subject prefix of the events.
func (s *Server) busEventsPrefix() string {
	if p := s.config.Bus.Events.SubjectPrefix; p != "" {
		return p
	}
	return bus.DefaultSubjectPrefix
}

// cdcReadableStreams are the change streams a reader without a management
// token reads through its consumer; the admin event stream only a
// management token reads through the JetStream API.
func cdcReadableStreams(region string) []string {
	return []string{bus.EventStreamName(region), bus.ConsulKVChangesStreamName(region)}
}

// cdcConsumerName is the name of the consumer the servers keep for a reader:
// the accessor ID of a token, <allocation ID>_<task> of a workload identity
// (the task as busTaskToken writes it). Empty for the anonymous token and the
// leader, which get no consumer.
func cdcConsumerName(kind, name string) string {
	switch {
	case kind == bus.KindWorkload:
		alloc, task, _ := strings.Cut(name, "/")
		return alloc + "_" + busTaskToken(task, consumerNameToken)
	case kind == bus.KindToken && name != "anonymous" && name != "leader" && name != "":
		return consumerNameToken(name)
	}
	return ""
}

// busTaskToken writes a task name as a token of a subject or of a consumer
// name: replace changes the characters the token can't hold. A changed name
// gets "-" and the first eight hex digits of the SHA-256 of the task name
// appended, so that two tasks of an allocation whose names differ only in
// such characters ("a.b" and "a_b") get different tokens: they would
// otherwise share a consumer, or read each other's replies. A name without
// such characters stays as it is.
func busTaskToken(task string, replace func(string) string) string {
	token := replace(task)
	if token == task {
		return token
	}
	sum := sha256.Sum256([]byte(task))
	return token + "-" + hex.EncodeToString(sum[:4])
}

// consumerNameToken replaces the characters a consumer name can't hold.
func consumerNameToken(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}

// busInternalSubjects are the subjects a bus rule never gives, to subscribe
// or to publish: they belong to the servers and the nodes.
//   - $KV.>: the buckets, the Consul KV with the keys and sealed values of
//     every namespace among them (the Variables ACL rules those keys);
//   - $JS.>, $JSC.>, $NRG.>: the JetStream API, its acknowledgements and
//     the replication of the streams; a subscriber of $JS.API.> could
//     answer the requests of other connections;
//   - _R_.>: the deliveries of the consumers to the leaf readers;
//   - norca.telemetry.>: the usage reports of the nodes, which move the
//     autoscaler;
//   - $SYS.>: the services of the NATS system account that every account
//     imports (the information of the user and of the account); they
//     answer to the reply subject of the request;
//   - the inboxes of the servers' own connections (bus.InternalInboxPrefix);
//   - $NORCA.>: the subjects the servers answer requests of the reader
//     accounts on (bus.ReaderStreamInfoSubject).
var busInternalSubjects = []string{"$KV.>", "$JS.>", "$JSC.>", "$NRG.>", "_R_.>", "norca.telemetry.>",
	"$SYS.>", bus.InternalInboxPrefix + ".>", "$NORCA.>", riggerLogsAllRegions, "$O.>"}

// riggerLogsAllRegions are the subjects of the logs of the Rigger
// operations of every region: the servers write them, and they are read
// through the HTTP API, with the right to read operations, not from the bus.
const riggerLogsAllRegions = "norca.rigger.oplog.>"

// busGrantSubjects builds the subjects of a connection without every right
// from its bus and cdc rules:
//   - the bus rule's subjects outside the change streams and outside
//     JetStream and the other subjects of the servers (busInternalSubjects):
//     the change streams are read by the cdc rule only, and the JetStream
//     API of a reader is its own consumer, in the reader's account;
//   - the cdc rule's subjects inside the change streams, to subscribe live.
//
// dropped counts the subjects of the rules that were left out.
func (s *Server) busGrantSubjects(g *acl.BusGrant, cdc *acl.CDCGrant) (publish, subscribe []string, dropped int) {
	region := s.config.Region
	change := bus.ChangeStreamSubjects(s.busEventsPrefix(), region)
	collides := func(subject string, with []string) bool {
		return slices.ContainsFunc(with, func(w string) bool { return bus.SubjectsCollide(subject, w) })
	}
	// Neither side of a bus rule reaches the change streams or the subjects
	// of the servers and the nodes (busInternalSubjects).
	reserved := append(slices.Clone(change), busInternalSubjects...)

	for _, subject := range busRuleSubjects(g.Subscribe) {
		if !collides(subject, reserved) {
			subscribe = append(subscribe, subject)
		}
	}
	for _, subject := range busRulePublishSubjects(g.Publish) {
		if !collides(subject, reserved) {
			publish = append(publish, subject)
		}
	}
	// A cdc subject is granted as written when it reaches the change
	// streams; the other subjects of the servers under "norca" (the
	// telemetry of the nodes, the logs of the Rigger operations) are denied
	// (busNonChangeDeny), so that "norca.>" gives the changes only.
	var receive []string
	for _, subject := range busRuleSubjects(cdc.Receive) {
		if collides(subject, change) {
			receive = append(receive, subject)
		}
	}
	dropped = len(g.Subscribe) + len(g.Publish) + len(cdc.Receive) - len(subscribe) - len(publish) - len(receive)
	subscribe = append(subscribe, receive...)

	// No subject of JetStream: a request names its reply subject, which
	// NATS does not check, and JetStream would write its answer to any
	// subject of this account. The consumers are read in the reader's own
	// account (busReaderGrant).
	slices.Sort(publish)
	slices.Sort(subscribe)
	return slices.Compact(publish), slices.Compact(subscribe), dropped
}

// busReaderGrant is the grant of a change data capture reader that connects
// directly: an identity with a cdc rule and no bus rule, or one that logged
// in as the reader (bus.ReaderUser). The connection is put into an account
// of its own (bus.ReaderAccount), as a leaf node is: the account imports the
// subjects of the cdc rule inside the change streams, live, and the
// JetStream subjects of the reader's own consumer of each stream it may
// read (information, fetch, acknowledgements) with the information of the
// account and of those streams. Nothing the reader publishes reaches a
// subject of the bus account, and neither does a reply subject it names:
// the answer of a request across the accounts comes back into the reader's
// account.
func (s *Server) busReaderGrant(aclObj *acl.ACL, kind, name string) (*bus.Grant, error) {
	cdc := aclObj.CDCGrant()
	if cdc.Empty() {
		return nil, errors.New("the token has no cdc rights: it is no change data capture reader")
	}
	streams, err := s.cdcLeafStreams(aclObj, cdc, false)
	if err != nil {
		return nil, err
	}
	consumer := cdcConsumerName(kind, name)
	spec := &bus.ReaderSpec{Reader: consumer, Info: consumer != ""}
	if consumer == "" {
		// The anonymous token: the changes live, no consumer.
		spec.Reader = consumerNameToken(name)
	}
	for _, st := range streams {
		spec.Subjects = append(spec.Subjects, st.subjects...)
		if st.consumer && consumer != "" {
			spec.Consumers = append(spec.Consumers, bus.ConsumerRef{Stream: st.name, Name: consumer})
		}
	}
	account, err := s.bus.ReaderAccount(spec)
	if err != nil {
		return nil, err
	}
	publish := spec.InfoSubjects()
	for _, c := range spec.Consumers {
		publish = append(publish, spec.ConsumerSubjects(c)...)
	}
	slices.Sort(publish)
	return &bus.Grant{
		Kind:    kind,
		Name:    name,
		Account: account,
		// The account holds what the reader may receive and its own
		// replies, whatever inbox it uses, and nothing else.
		Publish:   publish,
		Subscribe: []string{">"},
	}, nil
}

// busNonChangeDeny are the subjects of the servers that are not change
// streams: the telemetry of the nodes and the logs of the Rigger operations.
// A connection without every right never receives them, whatever a cdc or
// bus rule says; NATS withholds them also from a wider subscription such as
// "norca.>". Only the nodes publish telemetry, and only the servers write
// the logs and read both.
func (s *Server) busNonChangeDeny() []string {
	return []string{
		"norca.telemetry.>",
		riggerLogsAllRegions,
	}
}

// cdcCleanupConsumers removes the change data capture consumers whose reader
// lost its right: a token deleted or expired, an allocation ended, or a cdc
// rule that no longer covers every filter (the admin stream: no management
// token). The inactive threshold of the stream removes the rest. It runs on
// the leader with the revalidation of the bus connections.
func (s *Server) cdcCleanupConsumers() {
	if s.bus == nil || !s.bus.JetStreamReady() {
		return
	}
	region := s.config.Region
	streams := append(cdcReadableStreams(region), bus.AdminEventStreamName(region))
	err := s.withCDCJetStream(func(ctx context.Context, js jetstream.JetStream) error {
		for _, name := range streams {
			stream, err := js.Stream(ctx, name)
			if err != nil {
				continue // not made yet
			}
			admin := name == bus.AdminEventStreamName(region)
			lister := stream.ListConsumers(ctx)
			for info := range lister.Info() {
				reader, ok := info.Config.Metadata[cdcReaderMeta]
				if !ok {
					continue // not a consumer of a reader
				}
				filters := info.Config.FilterSubjects
				if len(filters) == 0 && info.Config.FilterSubject != "" {
					filters = []string{info.Config.FilterSubject}
				}
				if why := s.cdcReaderLost(reader, filters, admin); why != "" {
					if err := stream.DeleteConsumer(ctx, info.Name); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
						s.logger.Warn("change data capture: removing a consumer failed", "consumer", info.Name, "error", err)
						continue
					}
					s.logger.Info("change data capture: removed the consumer of a reader that lost its right",
						"stream", name, "consumer", info.Name, "reader", reader, "reason", why)
				}
			}
			if err := lister.Err(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.logger.Debug("change data capture: the cleanup of the consumers did not finish", "error", err)
	}
}

// cdcReaderLost returns why the reader of a consumer (metadata <kind>:<name>)
// may no longer read it, or "" when it still may. The consumers of a leaf
// node (kind leaf, the accessor of its token) also need the leaf right.
func (s *Server) cdcReaderLost(reader string, filters []string, admin bool) string {
	kind, name, _ := strings.Cut(reader, ":")
	snap := s.fsm.State()
	var aclObj *acl.ACL
	switch kind {
	case bus.KindToken, bus.KindLeaf:
		token, err := snap.ACLTokenByAccessorID(nil, name)
		if err != nil {
			return ""
		}
		if token == nil {
			return "token deleted"
		}
		if token.IsExpired(time.Now().UTC()) {
			return "token expired"
		}
		if aclObj, err = s.ResolveACLForToken(token); err != nil {
			return ""
		}
	case bus.KindWorkload:
		allocID, task, _ := strings.Cut(name, "/")
		alloc, err := snap.AllocByID(nil, allocID)
		if err != nil {
			return ""
		}
		if alloc == nil || alloc.ClientTerminalStatus() {
			return "allocation ended"
		}
		claims := &structs.IdentityClaims{
			Namespace: alloc.Namespace, JobID: alloc.JobID, AllocationID: alloc.ID, TaskName: task,
		}
		if aclObj, err = s.ResolveClaims(claims); err != nil {
			return ""
		}
	default:
		return ""
	}
	grant := aclObj.CDCGrant()
	if grant.All {
		return ""
	}
	if kind == bus.KindLeaf && !grant.Leaf {
		return "the cdc rule no longer allows a leaf node"
	}
	if admin {
		return "the admin events need a management token"
	}
	for _, filter := range filters {
		if !slices.ContainsFunc(grant.Receive, func(p string) bool { return bus.SubjectIsSubsetMatch(filter, p) }) {
			return "the cdc rule no longer covers " + filter
		}
	}
	return ""
}
