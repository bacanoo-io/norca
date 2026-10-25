// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"
)

// ErrDenied is returned (wrapped) when an intention, or the default policy,
// denies a connection.
var ErrDenied = errors.New("connection denied by service intentions")

// Source is the source side of a connection being authorized.
type Source struct {
	Namespace string
	Services  []string
}

// Decision is the result of evaluating intentions.
type Decision struct {
	// Allowed is whether the connection is allowed.
	Allowed bool

	// Intention is the intention that decided, or nil if the default
	// policy did.
	Intention *api.ServiceIntention
}

// precedence returns how specific an intention is: an exact destination
// counts most, then an exact source namespace, then an exact source service.
// It returns -1 if the intention doesn't match.
func precedence(i *api.ServiceIntention, src Source, srcService, dstNamespace, dstService string) int {
	if i == nil || i.Namespace != dstNamespace {
		return -1
	}
	score := 0
	switch {
	case i.Destination == api.ServiceIntentionWildcard:
	case strings.EqualFold(i.Destination, dstService):
		score += 4
	default:
		return -1
	}
	srcNamespace := i.SourceNamespace
	if srcNamespace == "" {
		srcNamespace = i.Namespace
	}
	switch {
	case srcNamespace == api.ServiceIntentionWildcard:
	case srcNamespace == src.Namespace:
		score += 2
	default:
		return -1
	}
	switch {
	case i.Source == api.ServiceIntentionWildcard:
	case srcService != "" && strings.EqualFold(i.Source, srcService):
		score++
	default:
		return -1
	}
	return score
}

// Evaluate decides whether a source may connect to a destination service of
// dstNamespace. The most specific matching intention decides; at equal
// specificity deny wins over allow. A source with several services matches
// an intention through any of them, and a source with no service only
// matches intentions with a wildcard source. Without a matching intention
// the default policy applies.
func Evaluate(intentions []*api.ServiceIntention, src Source, dstNamespace, dstService string, defaultAllow bool) Decision {
	best := -1
	var decision *api.ServiceIntention
	consider := func(i *api.ServiceIntention, score int) {
		switch {
		case score > best:
			best, decision = score, i
		case score == best && decision != nil &&
			decision.Action != api.ServiceIntentionActionDeny && i.Action == api.ServiceIntentionActionDeny:
			decision = i
		}
	}
	for _, i := range intentions {
		if len(src.Services) == 0 {
			if score := precedence(i, src, "", dstNamespace, dstService); score >= 0 {
				consider(i, score)
			}
			continue
		}
		for _, service := range src.Services {
			if score := precedence(i, src, service, dstNamespace, dstService); score >= 0 {
				consider(i, score)
			}
		}
	}
	if decision == nil {
		return Decision{Allowed: defaultAllow}
	}
	return Decision{Allowed: decision.Action == api.ServiceIntentionActionAllow, Intention: decision}
}

// IntentionSource provides the current intentions of a namespace.
type IntentionSource interface {
	Intentions() []*api.ServiceIntention
}

// StaticIntentions is a fixed set of intentions.
type StaticIntentions []*api.ServiceIntention

// Intentions returns the intentions.
func (s StaticIntentions) Intentions() []*api.ServiceIntention { return s }

// Authorizer authorizes connections to the services of a namespace with the
// service intentions.
type Authorizer struct {
	// Namespace is the namespace of the destination services.
	Namespace string

	// Intentions provides the intentions of the namespace.
	Intentions IntentionSource

	// DefaultAllow allows connections no intention matches. The default is
	// to deny them.
	DefaultAllow bool
}

// Authorize returns nil if the peer may connect to the destination service,
// or an error wrapping ErrDenied.
func (a *Authorizer) Authorize(peer *Peer, destination string) error {
	if peer == nil {
		return fmt.Errorf("%w: no peer identity", ErrDenied)
	}
	if a == nil {
		return nil
	}
	var intentions []*api.ServiceIntention
	if a.Intentions != nil {
		intentions = a.Intentions.Intentions()
	}
	d := Evaluate(intentions, Source{Namespace: peer.ID.Namespace, Services: peer.Services}, a.Namespace, destination, a.DefaultAllow)
	if d.Allowed {
		return nil
	}
	if d.Intention != nil {
		return fmt.Errorf("%w: %s to %s/%s is denied by intention %s/%s -> %s", ErrDenied,
			peer.ID, a.Namespace, destination, d.Intention.SourceNamespace, d.Intention.Source, d.Intention.Destination)
	}
	return fmt.Errorf("%w: no intention allows %s to %s/%s", ErrDenied, peer.ID, a.Namespace, destination)
}

// IntentionWatcher keeps the intentions of a namespace up to date with
// blocking queries and serves them from memory.
type IntentionWatcher struct {
	client    *api.Client
	namespace string

	// WaitTime is the maximum duration of a blocking query (default 5m).
	WaitTime time.Duration

	mu         sync.RWMutex
	intentions []*api.ServiceIntention
	index      uint64
	err        error
	ready      chan struct{}
	readyOnce  sync.Once
}

// NewIntentionWatcher returns a watcher of the intentions of a namespace.
// The client should authenticate with a token that may read them, for
// example the workload identity of the task.
func NewIntentionWatcher(client *api.Client, namespace string) *IntentionWatcher {
	return &IntentionWatcher{
		client:    client,
		namespace: namespace,
		WaitTime:  5 * time.Minute,
		ready:     make(chan struct{}),
	}
}

// Run fetches the intentions until the context is cancelled. Failures are
// retried with backoff; the last known intentions stay in use.
func (w *IntentionWatcher) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		w.mu.RLock()
		index := w.index
		w.mu.RUnlock()

		q := (&api.QueryOptions{
			Namespace: w.namespace,
			WaitIndex: index,
			WaitTime:  w.WaitTime,
		}).WithContext(ctx)
		intentions, meta, err := w.client.ServiceIntentions().List(q)
		if err != nil {
			w.mu.Lock()
			w.err = err
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, time.Minute)
			continue
		}
		backoff = time.Second

		w.mu.Lock()
		w.err = nil
		if meta.LastIndex < w.index {
			// The servers were restored to an older state: start over.
			w.index = 0
		} else {
			w.index = meta.LastIndex
		}
		w.intentions = intentions
		w.mu.Unlock()
		w.readyOnce.Do(func() { close(w.ready) })
	}
}

// WaitReady blocks until the first fetch succeeded or the context ends.
func (w *IntentionWatcher) WaitReady(ctx context.Context) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		w.mu.RLock()
		defer w.mu.RUnlock()
		if w.err != nil {
			return fmt.Errorf("%w: %v", ctx.Err(), w.err)
		}
		return ctx.Err()
	}
}

// Intentions returns the last known intentions.
func (w *IntentionWatcher) Intentions() []*api.ServiceIntention {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.intentions
}

// Err returns the error of the last fetch, if it failed.
func (w *IntentionWatcher) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}
