// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"errors"
	"net/http"
	"sync"

	"github.com/hashicorp/go-hclog"
	"github.com/nats-io/nats.go"
)

// ErrNoBus is returned by Backend.BusConn when the client has no system bus
// to use: it is disabled on the client, or the servers run without one.
var ErrNoBus = errors.New("the servers run no system bus")

// autoKV is the backend "auto": the bucket of the system bus when the
// servers run one, Variables otherwise. The choice is made by the first
// request that finds out (the client knows its servers after the first
// heartbeat) and kept: the keys must not move between two stores.
type autoKV struct {
	logger    hclog.Logger
	busConn   func() (*nats.Conn, error)
	bus       func() kvStore
	variables func() kvStore

	mu    sync.Mutex
	store kvStore
}

// resolve returns the chosen store, choosing it on first use.
func (a *autoKV) resolve() (kvStore, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		return a.store, nil
	}
	_, err := a.busConn()
	switch {
	case err == nil:
		a.store = a.bus()
		a.logger.Info("the Consul KV is kept on the system bus of the servers")
	case errors.Is(err, ErrNoBus):
		a.store = a.variables()
		a.logger.Info("the Consul KV is kept in Variables: the servers run no system bus")
	default:
		return nil, &codedError{http.StatusServiceUnavailable, "the KV is not available yet: " + err.Error()}
	}
	return a.store, nil
}

func (a *autoKV) Get(r *request, key string, prefix bool) ([]*kvEntry, uint64, error) {
	s, err := a.resolve()
	if err != nil {
		return nil, 0, err
	}
	return s.Get(r, key, prefix)
}

func (a *autoKV) Put(r *request, e *kvEntry, cas *uint64) (bool, error) {
	s, err := a.resolve()
	if err != nil {
		return false, err
	}
	return s.Put(r, e, cas)
}

func (a *autoKV) Delete(r *request, key string, recurse bool, cas *uint64) (bool, error) {
	s, err := a.resolve()
	if err != nil {
		return false, err
	}
	return s.Delete(r, key, recurse, cas)
}

func (a *autoKV) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.store != nil {
		a.store.Close()
	}
}
