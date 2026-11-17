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
	"time"

	"github.com/nats-io/jwt/v2"
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

	// Domain, when set, is the JetStream domain the reader names in the
	// requests to its consumers ($JS.<Domain>.API.CONSUMER...): the NATS
	// server of a leaf node answers $JS.API itself when it runs JetStream.
	Domain string

	// Leaf limits the account to one leaf node connection and no client
	// connection, with ReaderMaxSubscriptions and ReaderMaxPayload.
	Leaf bool

	// Info lets the reader also ask for the information of the bus account
	// and of the streams of its consumers ($JS.API.INFO,
	// $JS.API.STREAM.INFO.<stream>), which the JetStream clients ask for
	// before they read a consumer: a reader that connects directly, not a
	// leaf node.
	Info bool
}

const (
	// ReaderMaxSubscriptions bounds the subscriptions of a leaf account: the
	// interest of every client behind the leaf node counts.
	ReaderMaxSubscriptions = 10000

	// ReaderMaxPayload bounds what a leaf node may publish into its
	// account: the fetch requests and acknowledgements of its consumers are
	// small.
	ReaderMaxPayload = 4096

	// ReaderResponseThreshold is how long the bus account keeps the way back
	// of a fetch of a reader account (the default of nats-server is two
	// minutes). A fetch waits at most ReaderMaxFetchExpires, which the
	// consumers of a reader account must not exceed (MaxRequestExpires).
	ReaderResponseThreshold = 35 * time.Second
	ReaderMaxFetchExpires   = 30 * time.Second

	// ReaderMaxPendingResponses bounds the requests of a reader account
	// whose way back the bus account still keeps: every fetch across the
	// accounts leaves one until ReaderResponseThreshold, whatever the
	// consumer answers, and nats-server does not bound them. A client that
	// fetches 900 times a second stays below it; a leaf above it is cut off
	// for ReaderBanTime.
	ReaderMaxPendingResponses = 32768
	ReaderBanTime             = 30 * time.Second

	// readerWatchInterval is how often the fetches of the reader accounts
	// are counted.
	readerWatchInterval = 2 * time.Second
)

// reader is a reader account made on the embedded server.
type reader struct {
	spec *ReaderSpec

	// bannedUntil refuses the connections of the account until then: it had
	// more than the allowed requests waiting.
	bannedUntil time.Time
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
	fmt.Fprintf(h, "d:%s\nl:%t\n", spec.Domain, spec.Leaf)
	if spec.Info {
		fmt.Fprint(h, "i\n")
	}
	return ReaderAccountPrefix + spec.Reader + "_" + hex.EncodeToString(h.Sum(nil))[:12]
}

// APIPrefix is the prefix of the JetStream API subjects of the reader:
// "$JS.API", or "$JS.<domain>.API".
func (spec *ReaderSpec) APIPrefix() string {
	if spec.Domain == "" {
		return "$JS.API"
	}
	return "$JS." + spec.Domain + ".API"
}

// consumerImport is a subject of the JetStream API a reader account imports
// for one of its consumers: from is the subject in the reader account, to
// the one in the bus account; streamed is set when an answer has many
// messages (a fetch).
type consumerImport struct {
	from, to string
	streamed bool
}

func (spec *ReaderSpec) consumerImports(c ConsumerRef) []consumerImport {
	api, consumer := spec.APIPrefix(), c.Stream+"."+c.Name
	return []consumerImport{
		{api + ".CONSUMER.MSG.NEXT." + consumer, "$JS.API.CONSUMER.MSG.NEXT." + consumer, true},
		{api + ".CONSUMER.INFO." + consumer, "$JS.API.CONSUMER.INFO." + consumer, false},
		// The acknowledgements go to the reply subject of a delivered
		// message, which names no domain.
		{"$JS.ACK." + consumer + ".>", "$JS.ACK." + consumer + ".>", false},
	}
}

// infoImports are the information subjects of the JetStream API a reader
// account with Info imports: the account and each stream of its consumers.
func (spec *ReaderSpec) infoImports() []consumerImport {
	if !spec.Info {
		return nil
	}
	api := spec.APIPrefix()
	out := []consumerImport{{api + ".INFO", "$JS.API.INFO", false}}
	var streams []string
	for _, c := range spec.Consumers {
		if !slices.Contains(streams, c.Stream) {
			streams = append(streams, c.Stream)
			out = append(out, consumerImport{api + ".STREAM.INFO." + c.Stream, "$JS.API.STREAM.INFO." + c.Stream, false})
		}
	}
	return out
}

// InfoSubjects are the subjects a reader with Info publishes to for the
// information of the account and of the streams of its consumers.
func (spec *ReaderSpec) InfoSubjects() []string {
	var out []string
	for _, imp := range spec.infoImports() {
		out = append(out, imp.from)
	}
	return out
}

// ConsumerSubjects are the subjects a reader publishes to for its consumer
// c: fetch, information and acknowledgements.
func (spec *ReaderSpec) ConsumerSubjects(c ConsumerRef) []string {
	var out []string
	for _, imp := range spec.consumerImports(c) {
		out = append(out, imp.from)
	}
	return out
}

// validName checks a name that becomes a token of a subject or account.
func validName(name string) bool {
	return name != "" && !strings.ContainsAny(name, ". *>\t\r\n")
}

// validReaderSpec checks a spec against the change streams of the bus.
func (s *Server) validReaderSpec(spec *ReaderSpec) error {
	if spec == nil || spec.Reader == "" {
		return errors.New("bus: a reader account needs a reader")
	}
	if !validName(spec.Reader) {
		return fmt.Errorf("bus: invalid reader name %q", spec.Reader)
	}
	if len(spec.Subjects) == 0 && len(spec.Consumers) == 0 {
		return errors.New("bus: a reader account imports nothing")
	}
	if spec.Domain != "" && !validName(spec.Domain) {
		return fmt.Errorf("bus: invalid domain %q", spec.Domain)
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
		if !validName(c.Name) {
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
// connection of the reader makes it again. Calls are serialized, so a
// connection never lands in an account whose imports are still being made;
// a leaf account whose making failed half way is made again by the next
// call.
func (s *Server) ReaderAccount(spec *ReaderSpec) (string, error) {
	if err := s.validReaderSpec(spec); err != nil {
		return "", err
	}
	name := ReaderAccountName(spec)

	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	ns := s.ns
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	if s.readersOf != ns {
		s.readers, s.readersOf = map[string]*reader{}, ns
	}
	if _, ok := s.readers[name]; ok {
		return name, nil
	}

	bus, err := ns.LookupAccount(AccountName)
	if err != nil {
		return "", fmt.Errorf("bus: the account %s: %w", AccountName, err)
	}
	acc, isNew := ns.LookupOrRegisterAccount(name)
	switch {
	case spec.Leaf:
		// The limits of a leaf account. The claims replace the imports and
		// exports of the account, also those of an earlier call that failed
		// half way, so they are set before the imports.
		claims := jwt.NewAccountClaims(name)
		claims.Limits.Conn = 0
		claims.Limits.LeafNodeConn = 1
		claims.Limits.Subs = ReaderMaxSubscriptions
		claims.Limits.Payload = ReaderMaxPayload
		ns.UpdateAccountClaims(acc, claims)
	case !isNew:
		// Without claims nothing removes the imports of an earlier call
		// (claims would also answer the JetStream API of an account without
		// JetStream before the imports do).
		return "", fmt.Errorf("bus: the account %s was not made completely; it stays until the bus restarts", name)
	}
	for _, sub := range spec.Subjects {
		if err := bus.AddStreamExport(sub, []*natsserver.Account{acc}); err != nil {
			return "", fmt.Errorf("bus: exporting %q: %w", sub, err)
		}
		if err := acc.AddStreamImport(bus, sub, ""); err != nil {
			return "", fmt.Errorf("bus: importing %q: %w", sub, err)
		}
	}
	imports := [][]consumerImport{spec.infoImports()}
	for _, c := range spec.Consumers {
		imports = append(imports, spec.consumerImports(c))
	}
	for _, group := range imports {
		for _, imp := range group {
			resp := natsserver.Singleton
			if imp.streamed {
				resp = natsserver.Streamed
			}
			if err := bus.AddServiceExportWithResponse(imp.to, resp, []*natsserver.Account{acc}); err != nil {
				return "", fmt.Errorf("bus: exporting %q: %w", imp.to, err)
			}
			if err := acc.AddServiceImport(bus, imp.from, imp.to); err != nil {
				return "", fmt.Errorf("bus: importing %q: %w", imp.from, err)
			}
			if imp.streamed {
				// The way back of a fetch is kept for the longest wait of a
				// fetch, not for the two minutes of nats-server.
				if err := bus.SetServiceExportResponseThreshold(imp.to, s.responseThreshold); err != nil {
					return "", fmt.Errorf("bus: the response threshold of %q: %w", imp.to, err)
				}
			}
		}
	}
	made := *spec
	made.Subjects, made.Consumers = slices.Clone(spec.Subjects), slices.Clone(spec.Consumers)
	s.readers[name] = &reader{spec: &made}
	s.logger.Debug("reader account made", "account", name, "subjects", spec.Subjects,
		"consumers", len(spec.Consumers), "leaf", spec.Leaf)
	return name, nil
}
