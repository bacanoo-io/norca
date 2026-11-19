// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
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

	// readerRetireGrace is how long a reader account stays without a
	// connection before it is retired (retireReader). It is well above the
	// time a client or a leaf node takes to reconnect (seconds), and above
	// ReaderBanTime and ReaderResponseThreshold, so a reader that lost its
	// connection or was cut off comes back into its own account; a reader
	// that comes later gets a new one, which costs nothing it would notice.
	readerRetireGrace = 5 * time.Minute

	// readerSweepInterval is how often the reader accounts are looked at
	// for retirement and compaction.
	readerSweepInterval = time.Minute

	// Config.ReaderCompactThreshold and readerCompactInterval decide when
	// the retired reader accounts are dropped (compactReaders): at least
	// ReaderCompactThreshold of them (DefaultReaderCompactThreshold), more
	// than the live ones, and not within readerCompactInterval of the last
	// compaction. A compaction reloads the options of the embedded server,
	// which closes every client connection of this server (they reconnect),
	// as a route change does, and the live readers make their accounts
	// again. Such a reconnection at most once an hour is worth the memory of
	// the retired accounts, and more than the live ones keep the retired
	// memory below that of the live accounts.
	readerCompactInterval = time.Hour
)

// reader is a reader account made on the embedded server.
type reader struct {
	spec *ReaderSpec

	// key is ReaderAccountName of the spec: one live account per key.
	key string

	// bannedUntil refuses the connections of the account until then: it had
	// more than the allowed requests waiting.
	bannedUntil time.Time

	// used is when the account last had a connection or was asked for.
	used time.Time
}

// ConsumerRef names a consumer of a stream.
type ConsumerRef struct {
	Stream string
	Name   string
}

// ReaderAccountPrefix starts the name of every reader account.
const ReaderAccountPrefix = "CDC_"

// ReaderAccountName is the name of the first account of a reader spec: the
// reader and a digest of what it imports, so a changed spec gets a new
// account (nats-server can't remove an import). A connection of the old spec
// is closed by Revalidate (its grant names the old account) and reconnects
// into the new one. An account made again for the spec while the retired
// one still exists gets a generation appended (_<n>).
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

// ReaderStreamInfoSubject is the subject, followed by the name of the
// stream, that the stream information requests of the reader accounts
// arrive on in the bus account. The Norca servers answer there with the
// configuration and the state of the stream; the JetStream API would also
// give every subject of the stream with its message count to a request that
// asks for it, and a stream holds the changes of every namespace.
const ReaderStreamInfoSubject = "$NORCA.READER.STREAM.INFO"

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
			// Not the JetStream API, which lists the subjects of the
			// stream on request: the servers answer (see
			// ReaderStreamInfoSubject).
			out = append(out, consumerImport{api + ".STREAM.INFO." + c.Stream, ReaderStreamInfoSubject + "." + c.Stream, false})
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
// An account without a connection for readerRetireGrace is retired
// (sweepReaders): it imports nothing and takes no connection, and the next
// call for its spec makes a new account, named with a generation. nats-server
// deletes no account at runtime: the retired accounts, and the exports of the
// bus account to them, stay until the options of the embedded server are
// reloaded (a compaction of the retired accounts, a route change, a TLS
// reload). A reload deletes every account made at runtime and resets the
// exports of the bus account; the connections of the reader accounts are
// closed (reload) and the next connection of a reader makes its account
// again. Calls are serialized, so a connection never lands in an account
// whose imports are still being made; an account whose making failed half
// way is retired.
func (s *Server) ReaderAccount(spec *ReaderSpec) (string, error) {
	if err := s.validReaderSpec(spec); err != nil {
		return "", err
	}
	key := ReaderAccountName(spec)

	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	ns := s.ns
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	if s.reloading {
		// nats-server authenticates connections during a reload, and deletes
		// the accounts made at runtime: an account made now could be lost
		// half way. The connection is refused and comes again.
		return "", errors.New("bus: the options of the embedded server are being reloaded; connect again")
	}
	s.readersOn(ns)
	now := time.Now()
	var old *reader
	if name, ok := s.readerKeys[key]; ok {
		r := s.readers[name]
		if _, err := ns.LookupAccount(name); err == nil {
			r.used = now
			return name, nil
		}
		// Gone with a reload of the options: made again, its ban kept.
		old = r
		s.forgetReader(name)
	}

	bus, err := ns.LookupAccount(AccountName)
	if err != nil {
		return "", fmt.Errorf("bus: the account %s: %w", AccountName, err)
	}
	name := s.freeReaderName(ns, key)
	acc, _ := ns.LookupOrRegisterAccount(name)
	if err := s.importReader(ns, bus, acc, spec); err != nil {
		// The claims remove the imports made so far.
		s.retireReader(ns, name, acc)
		return "", err
	}
	made := *spec
	made.Subjects, made.Consumers = slices.Clone(spec.Subjects), slices.Clone(spec.Consumers)
	r := &reader{spec: &made, key: key, used: now}
	if old != nil {
		r.bannedUntil = old.bannedUntil
	}
	s.readers[name], s.readerKeys[key] = r, name
	s.logger.Debug("reader account made", "account", name, "subjects", spec.Subjects,
		"consumers", len(spec.Consumers), "leaf", spec.Leaf)
	return name, nil
}

// freeReaderName returns the first name of the reader accounts of key that
// no account of ns has: key itself, or key_<generation>. The caller holds
// readerLock.
func (s *Server) freeReaderName(ns *natsserver.Server, key string) string {
	name := key
	for {
		if _, err := ns.LookupAccount(name); err != nil {
			return name
		}
		s.readerSeq++
		name = key + "_" + strconv.FormatUint(s.readerSeq, 10)
	}
}

// importReader gives the new account acc what spec imports from the bus
// account.
func (s *Server) importReader(ns *natsserver.Server, bus, acc *natsserver.Account, spec *ReaderSpec) error {
	if spec.Leaf {
		// The limits of a leaf account. The claims replace the imports and
		// exports of the account, so they are set before the imports.
		claims := jwt.NewAccountClaims(acc.GetName())
		claims.Limits.Conn = 0
		claims.Limits.LeafNodeConn = 1
		claims.Limits.Subs = ReaderMaxSubscriptions
		claims.Limits.Payload = ReaderMaxPayload
		ns.UpdateAccountClaims(acc, claims)
	}
	for _, sub := range spec.Subjects {
		if err := bus.AddStreamExport(sub, []*natsserver.Account{acc}); err != nil {
			return fmt.Errorf("bus: exporting %q: %w", sub, err)
		}
		if err := acc.AddStreamImport(bus, sub, ""); err != nil {
			return fmt.Errorf("bus: importing %q: %w", sub, err)
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
				return fmt.Errorf("bus: exporting %q: %w", imp.to, err)
			}
			if err := acc.AddServiceImport(bus, imp.from, imp.to); err != nil {
				return fmt.Errorf("bus: importing %q: %w", imp.from, err)
			}
			if imp.streamed {
				// The way back of a fetch is kept for the longest wait of a
				// fetch, not for the two minutes of nats-server.
				if err := bus.SetServiceExportResponseThreshold(imp.to, s.responseThreshold); err != nil {
					return fmt.Errorf("bus: the response threshold of %q: %w", imp.to, err)
				}
			}
		}
	}
	return nil
}

// readersOn starts the reader accounts of the embedded server ns afresh
// when it replaced the one they were made on: a restarted server has none.
// The caller holds readerLock.
func (s *Server) readersOn(ns *natsserver.Server) {
	if s.readersOf != ns {
		s.readers, s.readerKeys, s.readersOf = map[string]*reader{}, map[string]string{}, ns
		s.retiredReaders = 0
	}
}

// forgetReader drops the reader account name from the live ones. The
// caller holds readerLock.
func (s *Server) forgetReader(name string) {
	if r, ok := s.readers[name]; ok {
		if s.readerKeys[r.key] == name {
			delete(s.readerKeys, r.key)
		}
		delete(s.readers, name)
	}
}

// retireReader leaves the reader account name inert: its claims import
// nothing and allow no connection and no leaf node. nats-server can't delete
// an account at runtime; the account and the exports of the bus account to
// it stay until the next reload of the options (compactReaders). The caller
// holds readerLock.
func (s *Server) retireReader(ns *natsserver.Server, name string, acc *natsserver.Account) {
	claims := jwt.NewAccountClaims(name)
	claims.Limits.Conn = 0
	claims.Limits.LeafNodeConn = 0
	claims.Limits.Subs = 0
	claims.Limits.Imports = 0
	claims.Limits.Exports = 0
	ns.UpdateAccountClaims(acc, claims)
	s.forgetReader(name)
	s.retiredReaders++
}

// setReaderRetirement changes when the reader accounts are retired and
// compacted; for tests. The threshold of the compaction comes from
// Config.ReaderCompactThreshold; setReaderCompactThreshold changes it.
func (s *Server) setReaderRetirement(grace, compactInterval time.Duration) {
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	s.retireGrace, s.compactInterval = grace, compactInterval
}

// setReaderCompactThreshold changes the threshold of the compaction of a
// running server; for tests.
func (s *Server) setReaderCompactThreshold(n int) {
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	s.compactMin = n
}

// sweepReaders retires the reader accounts that had no connection for the
// grace period (readerRetireGrace), closes the connections whose reader
// account is no longer theirs (closeStaleReaders), and drops the retired
// accounts when they are due (compactReaders). A reader account exists only
// on the server its reader connected to, so every server sweeps its own.
// Whether the reader still has its right does not matter: an account without
// a connection holds nothing a reader would miss (its consumers live in the
// bus account), and a reader that comes back gets a new one.
func (s *Server) sweepReaders(now time.Time) {
	ns := s.server()
	var idle []string
	s.readerLock.Lock()
	if s.readersOf == ns && !s.reloading {
		for name, r := range s.readers {
			acc, err := ns.LookupAccount(name)
			if err != nil {
				// Deleted by a reload of the options: the next call for its
				// spec makes it again. Kept while its reader is cut off.
				if !now.Before(r.bannedUntil) {
					s.forgetReader(name)
				}
				continue
			}
			if acc.NumConnections() > 0 || acc.NumLeafNodes() > 0 {
				r.used = now
				continue
			}
			if now.Sub(r.used) >= s.retireGrace {
				idle = append(idle, name)
			}
		}
	}
	s.readerLock.Unlock()

	// One at a time: nats-server walks every account of the server for a
	// retirement (milliseconds with thousands of accounts), and the
	// authentication of the readers waits for readerLock.
	retired := 0
	for _, name := range idle {
		select {
		case <-s.stopCh:
			return
		default:
		}
		if s.retireIdle(ns, name, now) {
			retired++
		}
	}
	s.readerLock.Lock()
	compact := s.readersOf == ns && !s.reloading && s.compactDue(now)
	live, total := len(s.readers), s.retiredReaders
	s.readerLock.Unlock()
	if retired > 0 {
		s.logger.Debug("reader accounts without a connection retired", "retired", retired,
			"retired_total", total, "live", live)
	}
	s.closeStaleReaders(ns)
	if compact {
		s.compactReaders(now)
	}
}

// retireIdle retires the reader account name if it is still live, has no
// connection and was not used for the grace period, and reports whether it
// did.
func (s *Server) retireIdle(ns *natsserver.Server, name string, now time.Time) bool {
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	r, ok := s.readers[name]
	if !ok || s.readersOf != ns || s.reloading || now.Sub(r.used) < s.retireGrace {
		return false
	}
	acc, err := ns.LookupAccount(name)
	if err != nil || acc.NumConnections() > 0 || acc.NumLeafNodes() > 0 {
		return false
	}
	s.retireReader(ns, name, acc)
	return true
}

// readerSweepLoop sweeps the reader accounts (sweepReaders) until the bus
// shuts down. It is a loop of its own: a sweep may take long with many
// accounts, and must not hold up the check of the fetches (leafWatchLoop).
func (s *Server) readerSweepLoop() {
	ticker := time.NewTicker(readerSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			s.sweepReaders(now)
		}
	}
}

// compactDue reports whether the retired reader accounts are to be dropped.
// The caller holds readerLock.
func (s *Server) compactDue(now time.Time) bool {
	return s.retiredReaders >= s.compactMin && s.retiredReaders > len(s.readers) &&
		now.Sub(s.compactedAt) >= s.compactInterval
}

// compactReaders drops the retired reader accounts: it reloads the options
// of the embedded server, unchanged. nats-server then deletes every account
// made at runtime, resets the exports of the bus account to its
// configuration and closes the client connections (it does not know the
// users the custom authentication registered); reload closes the leaf nodes
// of the reader accounts. Every connection of this server reconnects, the
// readers into new accounts.
func (s *Server) compactReaders(now time.Time) {
	s.routesLock.Lock()
	defer s.routesLock.Unlock()
	// ns and opts change only under routesLock.
	ns, opts := s.ns, s.opts
	s.readerLock.Lock()
	// A reload since the sweep (a route change) may have dropped them.
	due := s.readersOf == ns && !s.reloading && s.compactDue(now)
	retired, live := s.retiredReaders, len(s.readers)
	if due {
		s.compactedAt = now
	}
	s.readerLock.Unlock()
	if !due {
		return
	}
	s.logger.Info("dropping the retired reader accounts of the system bus: the connections of this server reconnect",
		"retired", retired, "live", live)
	if err := s.reload(ns, opts.Clone()); err != nil {
		s.logger.Warn("failed to drop the retired reader accounts of the system bus", "error", err)
	}
}

// closeStaleReaders closes the connections of reader accounts that are no
// longer in the account they were registered in: it was deleted by a reload
// of the options, or made again under its name. nats-server closes the
// client connections at a reload itself, but keeps a leaf node in its
// deleted account, where it reaches none of its imports. A connection the
// server does not list yet (it is being authenticated) is looked at again by
// the next sweep.
func (s *Server) closeStaleReaders(ns *natsserver.Server) {
	type entry struct {
		id uint64
		c  *client
	}
	var readers []entry
	s.l.Lock()
	for id, c := range s.clients {
		if strings.HasPrefix(c.grant.Account, ReaderAccountPrefix) {
			readers = append(readers, entry{id, c})
		}
	}
	s.l.Unlock()

	closed := 0
	for _, e := range readers {
		if acc, err := ns.LookupAccount(e.c.grant.Account); err == nil && acc == e.c.acc {
			continue
		}
		if err := ns.DisconnectClientByID(e.id); err == nil {
			closed++
		} else if time.Since(e.c.at) < connectGrace {
			continue
		}
		s.l.Lock()
		if s.clients[e.id] == e.c {
			delete(s.clients, e.id)
		}
		s.l.Unlock()
	}
	if closed > 0 {
		s.logger.Info("closed the bus connections of reader accounts a reload dropped; they reconnect",
			"connections", closed)
	}
}
