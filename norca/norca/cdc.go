// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
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
// the accessor ID of a token, <allocation ID>_<task> of a workload identity.
// Empty for the anonymous token and the leader, which get no consumer.
func cdcConsumerName(kind, name string) string {
	switch {
	case kind == bus.KindWorkload:
		alloc, task, _ := strings.Cut(name, "/")
		return alloc + "_" + consumerNameToken(task)
	case kind == bus.KindToken && name != "anonymous" && name != "leader" && name != "":
		return consumerNameToken(name)
	}
	return ""
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

// busGrantSubjects builds the subjects of a connection without every right
// from its bus and cdc rules:
//   - the bus rule's subjects outside the change streams and outside
//     JetStream ($JS.>): the change streams are read by the cdc rule only,
//     and the JetStream API of a reader is its own consumer;
//   - the cdc rule's subjects inside the change streams, to subscribe live;
//   - for a reader with a consumer name, the JetStream subjects of its own
//     consumers of the streams it may read: info, next and acknowledgements.
//
// dropped counts the subjects of the rules that were left out.
func (s *Server) busGrantSubjects(g *acl.BusGrant, cdc *acl.CDCGrant, kind, name string) (publish, subscribe []string, dropped int) {
	region := s.config.Region
	change := bus.ChangeStreamSubjects(s.busEventsPrefix(), region)
	collides := func(subject string, with []string) bool {
		return slices.ContainsFunc(with, func(w string) bool { return bus.SubjectsCollide(subject, w) })
	}
	jetStream := append(slices.Clone(change), "$JS.>")

	for _, subject := range busRuleSubjects(g.Subscribe) {
		if !collides(subject, change) {
			subscribe = append(subscribe, subject)
		}
	}
	for _, subject := range busRuleSubjects(g.Publish) {
		if !collides(subject, jetStream) {
			publish = append(publish, subject)
		}
	}
	// A cdc subject is granted as written when it reaches the change
	// streams; the other subjects under the prefix (the telemetry of the
	// nodes, the pack push notices) are denied (busNonChangeDeny), so that
	// "norca.>" gives the changes only.
	var receive []string
	for _, subject := range busRuleSubjects(cdc.Receive) {
		if collides(subject, change) {
			receive = append(receive, subject)
		}
	}
	dropped = len(g.Subscribe) + len(g.Publish) + len(cdc.Receive) - len(subscribe) - len(publish) - len(receive)
	subscribe = append(subscribe, receive...)

	if consumer := cdcConsumerName(kind, name); consumer != "" && len(receive) > 0 {
		publish = append(publish, "$JS.API.INFO")
		for _, stream := range cdcReadableStreams(region) {
			publish = append(publish,
				"$JS.API.STREAM.INFO."+stream,
				"$JS.API.CONSUMER.INFO."+stream+"."+consumer,
				"$JS.API.CONSUMER.MSG.NEXT."+stream+"."+consumer,
				"$JS.ACK."+stream+"."+consumer+".>",
			)
		}
	}
	slices.Sort(publish)
	slices.Sort(subscribe)
	return slices.Compact(publish), slices.Compact(subscribe), dropped
}

// busNonChangeDeny are the subjects under the event prefix that are not
// change streams: the telemetry of the nodes and the pack push notices. A
// connection without every right never receives them, whatever a cdc or bus
// rule says; NATS withholds them also from a wider subscription such as
// "norca.>". Only the nodes publish telemetry and the servers read both.
func (s *Server) busNonChangeDeny() []string {
	prefix := s.busEventsPrefix()
	return []string{
		"norca.telemetry.>",
		prefix + "." + bus.PushSubject,
		prefix + "." + bus.PushSubject + ".>",
	}
}
