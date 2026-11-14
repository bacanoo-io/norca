// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// AccountName is the NATS account of the bus. The Norca servers, the nodes,
// the ACL tokens and the workload identities connect into it, and the
// streams of the bus live in it. A reader account (ReaderAccount) imports
// from it; it imports from no other account.
const AccountName = "NORCA"

// accountConfig enables JetStream for the bus account, with the limits of
// the server. The limits of an account can only be set through the
// configuration parser of nats-server.
const accountConfig = `accounts { ` + AccountName + ` { jetstream: enabled } }`

// storeReaderConfig is the bus account of the store reader (ReadStoreKV),
// with a user the connections without credentials log in as.
const storeReaderConfig = `accounts { ` + AccountName + ` { jetstream: enabled, users: [{user: "store-reader"}] } }
no_auth_user: "store-reader"`

// newAccounts returns the configured accounts of an embedded server.
func newAccounts() ([]*natsserver.Account, error) {
	o := &natsserver.Options{}
	if err := o.ProcessConfigString(accountConfig); err != nil {
		return nil, fmt.Errorf("bus: configuring the account %s: %w", AccountName, err)
	}
	return o.Accounts, nil
}

// ReaderSpec is what a reader account receives from the bus account: the
// subjects of the change streams it may receive live, and the consumers it
// may pull from. Nothing flows the other way: the bus account imports
// nothing, and a reader account exports nothing.
type ReaderSpec struct {
	// Reader names the reader, e.g. the accessor ID of its token; the
	// account is named after it.
	Reader string

	// Subjects are received live (stream imports). Each lies inside a
	// subject of a change stream of the region: the events, the admin
	// events, the Consul KV notices. The telemetry, the pack push notices,
	// the Consul KV values and the JetStream API are never exported.
	Subjects []string

	// Consumers are the durable consumers of change streams the reader may
	// read: fetch (MSG.NEXT), information and acknowledgements of each,
	// nothing else of the JetStream API.
	Consumers []ConsumerRef
}

// ConsumerRef names a consumer of a stream.
type ConsumerRef struct {
	Stream string
	Name   string
}

// ReaderAccountPrefix starts the name of every reader account.
const ReaderAccountPrefix = "CDC_"

// ReaderAccountName is the name of the account of a reader spec: the reader
// and a digest of what it imports, so a changed spec gets a new account
// (nats-server can't remove an import). A connection of the old spec is
// closed by Revalidate (its grant names the old account) and reconnects
// into the new one.
func ReaderAccountName(spec *ReaderSpec) string {
	h := sha256.New()
	for _, s := range spec.Subjects {
		fmt.Fprintf(h, "s:%s\n", s)
	}
	for _, c := range spec.Consumers {
		fmt.Fprintf(h, "c:%s.%s\n", c.Stream, c.Name)
	}
	return ReaderAccountPrefix + spec.Reader + "_" + hex.EncodeToString(h.Sum(nil))[:12]
}

// consumerImports are the subjects of the JetStream API a reader account
// imports for one consumer, and whether the answers are streamed: a fetch
// answers with many messages.
func consumerImports(c ConsumerRef) []struct {
	subject  string
	streamed bool
} {
	return []struct {
		subject  string
		streamed bool
	}{
		{"$JS.API.CONSUMER.MSG.NEXT." + c.Stream + "." + c.Name, true},
		{"$JS.API.CONSUMER.INFO." + c.Stream + "." + c.Name, false},
		{"$JS.ACK." + c.Stream + "." + c.Name + ".>", false},
	}
}

// validReaderSpec checks a spec against the change streams of the bus.
func (s *Server) validReaderSpec(spec *ReaderSpec) error {
	if spec == nil || spec.Reader == "" {
		return errors.New("bus: a reader account needs a reader")
	}
	if strings.ContainsAny(spec.Reader, ". *>\t\r\n") {
		return fmt.Errorf("bus: invalid reader name %q", spec.Reader)
	}
	if len(spec.Subjects) == 0 && len(spec.Consumers) == 0 {
		return errors.New("bus: a reader account imports nothing")
	}
	prefix, region := s.config.Events.SubjectPrefix, s.config.Region
	within := ChangeStreamSubjects(prefix, region)
	for _, sub := range spec.Subjects {
		if !natsserver.IsValidSubject(sub) {
			return fmt.Errorf("bus: invalid subject %q", sub)
		}
		if !slices.ContainsFunc(within, func(p string) bool { return SubjectIsSubsetMatch(sub, p) }) {
			return fmt.Errorf("bus: %q is not inside a change stream of the region", sub)
		}
	}
	streams := []string{EventStreamName(region), AdminEventStreamName(region), ConsulKVChangesStreamName(region)}
	for _, c := range spec.Consumers {
		if !slices.Contains(streams, c.Stream) {
			return fmt.Errorf("bus: %q is not a change stream of the region", c.Stream)
		}
		if c.Name == "" || strings.ContainsAny(c.Name, ". *>\t\r\n") {
			return fmt.Errorf("bus: invalid consumer name %q", c.Name)
		}
	}
	return nil
}

// ReaderAccount makes the account of a reader on the running bus, if it
// does not exist yet, and returns its name for the Grant of the reader's
// connection. The bus account exports each subject and consumer API subject
// of the spec to this account only; the account imports them and nothing
// else, so a connection in it reaches no other subject of the bus: no
// inbox of another connection, no Consul KV value, no telemetry, no
// JetStream API but its own consumers.
//
// The account lives until the embedded server restarts: then the next
// connection of the reader makes it again.
func (s *Server) ReaderAccount(spec *ReaderSpec) (string, error) {
	if err := s.validReaderSpec(spec); err != nil {
		return "", err
	}
	name := ReaderAccountName(spec)

	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	ns := s.ns
	bus, err := ns.LookupAccount(AccountName)
	if err != nil {
		return "", fmt.Errorf("bus: the account %s: %w", AccountName, err)
	}
	acc, isNew := ns.LookupOrRegisterAccount(name)
	if !isNew {
		return name, nil
	}
	for _, sub := range spec.Subjects {
		if err := bus.AddStreamExport(sub, []*natsserver.Account{acc}); err != nil {
			return "", fmt.Errorf("bus: exporting %q: %w", sub, err)
		}
		if err := acc.AddStreamImport(bus, sub, ""); err != nil {
			return "", fmt.Errorf("bus: importing %q: %w", sub, err)
		}
	}
	for _, c := range spec.Consumers {
		for _, imp := range consumerImports(c) {
			resp := natsserver.Singleton
			if imp.streamed {
				resp = natsserver.Streamed
			}
			if err := bus.AddServiceExportWithResponse(imp.subject, resp, []*natsserver.Account{acc}); err != nil {
				return "", fmt.Errorf("bus: exporting %q: %w", imp.subject, err)
			}
			if err := acc.AddServiceImport(bus, imp.subject, ""); err != nil {
				return "", fmt.Errorf("bus: importing %q: %w", imp.subject, err)
			}
		}
	}
	s.logger.Debug("reader account made", "account", name, "subjects", spec.Subjects, "consumers", len(spec.Consumers))
	return name, nil
}
