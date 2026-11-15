// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/nats-io/nats.go/jetstream"
)

// Change data capture through leaf nodes: a NATS server outside the cluster
// attaches to the bus with an ACL token whose cdc rule allows a leaf
// (leaf = true). It gets an account of its own (bus.ReaderAccount) that
// imports the subjects of its rule inside the change streams, live, and its
// consumers: the servers make one consumer per stream for it,
// leaf-<accessor>, with the same subjects. The leaf publishes only the
// fetches and acknowledgements of those consumers; nothing it publishes
// reaches the bus account.

const (
	// cdcLeafEnsureInterval is how often the consumers of a connected leaf
	// node are made again when nothing changed: the inactive threshold of
	// the stream may have removed them.
	cdcLeafEnsureInterval = time.Minute

	// cdcLeafMaxWaiting bounds the fetches a leaf node may have waiting on
	// one consumer.
	cdcLeafMaxWaiting = 16
)

// cdcLeafDomain is the JetStream domain a leaf node names to reach its
// consumers ($JS.<domain>.API): its own NATS answers $JS.API itself when it
// runs JetStream.
func (s *Server) cdcLeafDomain() string {
	return "norca-" + bus.SubjectToken(s.config.Region)
}

// cdcLeafConsumerName is the name of the consumers of the leaf node of a
// token, also the reader of its account.
func cdcLeafConsumerName(accessor string) string {
	return "leaf-" + consumerNameToken(accessor)
}

// cdcLeafStream is a change stream a leaf node reads: its subjects inside
// the stream, which are also the filters of its consumer.
type cdcLeafStream struct {
	name     string
	subjects []string
	consumer bool
}

// cdcLeafStreams returns the subjects a leaf node with the cdc grant may
// receive, per change stream. A subject of the rule is narrowed to each
// stream (the rule "norca.>" gives "norca.<region>.*.*" of the events). The
// admin events: every topic with a management token, otherwise the live
// topics the token may see (the event stream API decides), without a
// consumer. Subjects that overlap without one containing the other are
// refused: a leaf would receive their events twice.
func (s *Server) cdcLeafStreams(aclObj *acl.ACL, grant *acl.CDCGrant) ([]cdcLeafStream, error) {
	prefix, region := s.busEventsPrefix(), s.config.Region
	// The subjects of a rule never start with a wildcard (busRuleSubjects);
	// a token with every right receives every change stream.
	receive := busRuleSubjects(grant.Receive)
	if grant.All {
		receive = []string{">"}
	}
	admin := prefix + "." + bus.AdminSubjectToken + "." + bus.SubjectToken(region)
	var adminSubjects []string
	if grant.All || aclObj.IsManagement() {
		adminSubjects = []string{admin + ".>"}
	} else {
		for _, t := range busAdminTopics {
			if t.allowed(aclObj) {
				adminSubjects = append(adminSubjects, admin+".*."+string(t.topic))
			}
		}
	}
	streams := []cdcLeafStream{
		{name: bus.EventStreamName(region), subjects: bus.EventStreamSubjects(prefix, region), consumer: true},
		{name: bus.ConsulKVChangesStreamName(region), subjects: bus.ConsulKVChangesSubjects(prefix, region), consumer: true},
		{name: bus.AdminEventStreamName(region), subjects: adminSubjects, consumer: grant.All},
	}

	var out []cdcLeafStream
	for _, st := range streams {
		var subjects []string
		for _, r := range receive {
			for _, within := range st.subjects {
				if sub, ok := bus.SubjectIntersect(r, within); ok {
					subjects = append(subjects, sub)
				}
			}
		}
		subjects = cdcWidest(subjects)
		if len(subjects) == 0 {
			continue
		}
		for i, a := range subjects {
			for _, b := range subjects[:i] {
				if bus.SubjectsCollide(a, b) {
					return nil, fmt.Errorf("the subjects %q and %q of the cdc rule overlap: a leaf node would receive their changes twice", b, a)
				}
			}
		}
		if st.consumer && len(subjects) > structs.CDCMaxFilters {
			return nil, fmt.Errorf("the cdc rule gives %d subjects of the stream %s, a consumer takes at most %d",
				len(subjects), st.name, structs.CDCMaxFilters)
		}
		out = append(out, cdcLeafStream{name: st.name, subjects: subjects, consumer: st.consumer})
	}
	if len(out) == 0 {
		return nil, errors.New("the cdc rule reaches no change stream of the region")
	}
	return out, nil
}

// cdcWidest leaves out the subjects another one of the list contains, and
// the duplicates; the order is kept.
func cdcWidest(subjects []string) []string {
	var out []string
	for i, a := range subjects {
		covered := false
		for j, b := range subjects {
			if i == j {
				continue
			}
			if a == b && j < i || a != b && bus.SubjectIsSubsetMatch(a, b) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, a)
		}
	}
	return out
}

// busLeafGrant authenticates a leaf node connection: an ACL token (not the
// anonymous one) whose cdc rule allows a leaf. It makes the leaf's account
// on this server and has its consumers made.
func (s *Server) busLeafGrant(c bus.Credentials) (*bus.Grant, error) {
	if !s.config.ACLEnabled {
		return nil, errors.New("a leaf node needs ACLs: it attaches with an ACL token and reads consumers named after it")
	}
	aclObj, kind, name, _, err := s.busIdentity(c.Token)
	if err != nil {
		return nil, err
	}
	if kind != bus.KindToken || name == "anonymous" || name == "leader" {
		return nil, errors.New("a leaf node attaches with an ACL token")
	}
	grant := aclObj.CDCGrant()
	if !grant.All && !grant.Leaf {
		return nil, errors.New("the cdc rule of the token allows no leaf node (leaf = true)")
	}
	streams, err := s.cdcLeafStreams(aclObj, grant)
	if err != nil {
		return nil, err
	}

	consumer := cdcLeafConsumerName(name)
	spec := &bus.ReaderSpec{Reader: consumer, Domain: s.cdcLeafDomain(), Leaf: true}
	for _, st := range streams {
		spec.Subjects = append(spec.Subjects, st.subjects...)
		if st.consumer {
			spec.Consumers = append(spec.Consumers, bus.ConsumerRef{Stream: st.name, Name: consumer})
		}
	}
	account, err := s.bus.ReaderAccount(spec)
	if err != nil {
		return nil, err
	}
	s.cdcLeafConsumers.ensure(s, name, streams)

	var publish []string
	for _, c := range spec.Consumers {
		publish = append(publish, spec.ConsumerSubjects(c)...)
	}
	return &bus.Grant{
		Kind:    bus.KindLeaf,
		Name:    name,
		Account: account,
		// One way: the leaf publishes only the fetches and the
		// acknowledgements of its consumers (and reads their information);
		// it receives what its account imports and its own replies.
		Publish:   publish,
		Subscribe: []string{">"},
	}, nil
}

// cdcLeafConsumers makes the consumers of the leaf nodes, in the
// background: the authentication of a connection does not wait for
// JetStream.
type cdcLeafConsumers struct {
	l    sync.Mutex
	made map[string]cdcLeafMade
}

type cdcLeafMade struct {
	digest string
	at     time.Time
}

// ensure makes the consumers of the leaf of a token, unless they were made
// with the same subjects within cdcLeafEnsureInterval.
func (m *cdcLeafConsumers) ensure(s *Server, accessor string, streams []cdcLeafStream) {
	var digest strings.Builder
	for _, st := range streams {
		if st.consumer {
			fmt.Fprintf(&digest, "%s:%s;", st.name, strings.Join(st.subjects, ","))
		}
	}
	m.l.Lock()
	if m.made == nil {
		m.made = map[string]cdcLeafMade{}
	}
	if last, ok := m.made[accessor]; ok && last.digest == digest.String() && time.Since(last.at) < cdcLeafEnsureInterval {
		m.l.Unlock()
		return
	}
	m.made[accessor] = cdcLeafMade{digest: digest.String(), at: time.Now()}
	m.l.Unlock()

	go func() {
		if err := s.cdcMakeLeafConsumers(accessor, streams); err != nil {
			s.logger.Warn("change data capture: making the consumers of a leaf node failed", "token", accessor, "error", err)
			m.l.Lock()
			delete(m.made, accessor) // the next connection tries again
			m.l.Unlock()
		}
	}()
}

// cdcMakeLeafConsumers creates or updates the consumers of the leaf of a
// token: durable pull consumers leaf-<accessor> with its subjects as
// filters, delivering the whole stream to a new consumer; a change keeps
// the position.
func (s *Server) cdcMakeLeafConsumers(accessor string, streams []cdcLeafStream) error {
	name := cdcLeafConsumerName(accessor)
	return s.withCDCJetStream(func(ctx context.Context, js jetstream.JetStream) error {
		var errs []error
		for _, st := range streams {
			if !st.consumer {
				continue
			}
			stream, err := js.Stream(ctx, st.name)
			if err != nil {
				errs = append(errs, fmt.Errorf("the stream %s: %w", st.name, err))
				continue
			}
			_, err = stream.Consumer(ctx, name)
			exists := err == nil
			if !exists {
				info, err := stream.Info(ctx)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if limit := info.Config.MaxConsumers; limit > 0 && info.State.Consumers >= limit {
					errs = append(errs, fmt.Errorf("the stream %s has its maximum of %d consumers (bus.events.max_consumers)", st.name, limit))
					continue
				}
			}
			_, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:            name,
				Description:        "change data capture consumer of the leaf node of token " + accessor,
				FilterSubjects:     st.subjects,
				DeliverPolicy:      jetstream.DeliverAllPolicy,
				AckPolicy:          jetstream.AckExplicitPolicy,
				AckWait:            cdcDefaultAckWait,
				MaxAckPending:      bus.EventsConsumerMaxAckPending,
				MaxWaiting:         cdcLeafMaxWaiting,
				MaxRequestBatch:    cdcMaxRequestBatch,
				MaxRequestMaxBytes: cdcMaxRequestMaxBytes,
				// A fetch across the accounts may wait as long as the bus
				// account keeps its way back.
				MaxRequestExpires: bus.ReaderMaxFetchExpires,
				Metadata:          map[string]string{cdcReaderMeta: bus.KindLeaf + ":" + accessor},
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("the consumer %s of %s: %w", name, st.name, err))
				continue
			}
			if !exists {
				s.logger.Info("change data capture: made the consumer of a leaf node", "stream", st.name,
					"consumer", name, "filters", st.subjects)
			}
		}
		return errors.Join(errs...)
	})
}
