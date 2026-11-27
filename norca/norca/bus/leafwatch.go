// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"time"
)

// A reader account reaches its consumers across the accounts. For every
// request with a reply subject nats-server keeps the way back in the bus
// account until the answer came (information, acknowledgement) or, for a
// fetch, until the response threshold of the export, and it does not bound
// their number: a leaf node that sends fetches as fast as it can would grow
// the memory of the server it attached to. leafWatchLoop counts them and
// cuts such a leaf off.

// readerBanned returns until when the connections of an account are
// refused; the zero time when they are not.
func (s *Server) readerBanned(account string) time.Time {
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	if r, ok := s.readers[account]; ok && time.Now().Before(r.bannedUntil) {
		return r.bannedUntil
	}
	return time.Time{}
}

// setReaderLimits changes the limits of the fetches of the reader accounts;
// for tests.
func (s *Server) setReaderLimits(maxPending int, threshold, ban time.Duration) {
	s.readerLock.Lock()
	defer s.readerLock.Unlock()
	s.maxPendingResponses, s.responseThreshold, s.leafBanTime = maxPending, threshold, ban
}

// leafWatchLoop checks the waiting requests of the reader accounts until
// the bus shuts down.
func (s *Server) leafWatchLoop() {
	ticker := time.NewTicker(readerWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkReaderLoad()
		}
	}
}

// checkReaderLoad closes the connections of the reader accounts that have
// more than maxPendingResponses requests waiting in the bus account, and
// refuses them for leafBanTime. Closing the connection drops its
// subscriptions, and with them nats-server drops the ways back.
func (s *Server) checkReaderLoad() {
	ns := s.server()
	bus, err := ns.LookupAccount(AccountName)
	if err != nil {
		return
	}
	// The whole account first: one map length.
	total := bus.NumPendingAllResponses()

	type over struct {
		account string
		pending int
	}
	var cut []over
	s.readerLock.Lock()
	limit, ban := s.maxPendingResponses, s.leafBanTime
	if total > limit && s.readersOf == ns {
		for name, r := range s.readers {
			pending := 0
			for _, c := range r.spec.Consumers {
				for _, imp := range r.spec.consumerImports(c) {
					pending += bus.NumPendingResponses(imp.to)
				}
			}
			if pending > limit {
				r.bannedUntil = time.Now().Add(ban)
				cut = append(cut, over{name, pending})
			}
		}
	}
	s.readerLock.Unlock()

	for _, o := range cut {
		var ids []uint64
		s.l.Lock()
		for id, c := range s.clients {
			if c.grant.Account == o.account {
				ids = append(ids, id)
			}
		}
		s.l.Unlock()
		s.logger.Warn("closing the connections of a reader account: too many of its requests wait for their answer",
			"account", o.account, "waiting", o.pending, "limit", limit, "refused_for", ban)
		for _, id := range ids {
			if err := ns.DisconnectClientByID(id); err != nil {
				s.logger.Debug("failed to close a bus connection", "connection_id", id, "error", err)
			}
		}
	}
}
