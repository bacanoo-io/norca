// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package vaultcompat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Task tokens of the endpoint.
//
// A token is bound to one task of one allocation: "nvc.<alloc ID>.<task
// name, base64url>.<MAC>", where the MAC is an HMAC-SHA256 of the allocation
// and task with a key derived from the node's secret. The client derives the
// token when the task's vault hook asks for one, and keeps the set of live
// tokens in memory only; nothing about tokens is written to the client's
// state. A token is accepted while it is live: from its derivation until the
// task stops (the vault hook stops its renewal) or, for a token that is not
// renewed, until its TTL ends.
//
// After an agent restart the vault hook of a running task renews the token
// it recovered from the task's directory; the MAC proves that this client
// derived it, so the token is adopted again for as long as the task runs,
// and the task keeps working with the token it has. Tokens of stopped tasks
// are never adopted again.

const tokenPrefix = "nvc."

var (
	errTokenInvalid = errors.New("permission denied")
	errTokenExpired = errors.New("permission denied: the token expired or was revoked")
)

// TaskInfo describes the task a token belongs to.
type TaskInfo struct {
	AllocID   string
	Namespace string
	JobID     string
	Group     string
	Task      string

	// Job is the allocation's job: its secrets defaults resolve the
	// lookups of the task.
	Job *structs.Job

	// Identity is the task's signed workload identity, used for every read.
	Identity string

	// Policies are the Vault policies of the task's vault block; they are
	// only shown by token lookups.
	Policies []string
}

// token is a live token.
type token struct {
	info     *TaskInfo
	accessor string
	created  time.Time
	expires  time.Time

	// renewed tokens are renewed by the client while the task runs.
	renewed bool

	limiter *rate.Limiter
}

// tokenStore holds the live tokens.
type tokenStore struct {
	key   []byte
	ttl   time.Duration
	limit rate.Limit
	burst int
	now   func() time.Time

	lock   sync.Mutex
	tokens map[string]*token
}

func newTokenStore(nodeSecret string, ttl time.Duration, limit float64, burst int) *tokenStore {
	mac := hmac.New(sha256.New, []byte(nodeSecret))
	mac.Write([]byte("norca vault-compatible endpoint token key"))
	return &tokenStore{
		key:    mac.Sum(nil),
		ttl:    ttl,
		limit:  rate.Limit(limit),
		burst:  burst,
		now:    time.Now,
		tokens: map[string]*token{},
	}
}

// mac returns the MAC of the token of a task.
func (s *tokenStore) mac(allocID, task string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(allocID))
	mac.Write([]byte{0})
	mac.Write([]byte(task))
	return mac.Sum(nil)
}

// tokenFor returns the token of a task.
func (s *tokenStore) tokenFor(allocID, task string) string {
	return tokenPrefix + allocID + "." + base64.RawURLEncoding.EncodeToString([]byte(task)) + "." +
		base64.RawURLEncoding.EncodeToString(s.mac(allocID, task))
}

// parse checks that the token was derived by this client and returns its
// allocation and task.
func (s *tokenStore) parse(tok string) (allocID, task string, ok bool) {
	rest, found := strings.CutPrefix(tok, tokenPrefix)
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	taskB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(mac, s.mac(parts[0], string(taskB))) {
		return "", "", false
	}
	return parts[0], string(taskB), true
}

// accessorOf returns the accessor of a token: a hash that identifies it in
// lookups without revealing it.
func accessorOf(tok string) string {
	sum := sha256.Sum256([]byte("accessor:" + tok))
	return hex.EncodeToString(sum[:12])
}

// derive registers the token of a task and returns it.
func (s *tokenStore) derive(info *TaskInfo) string {
	tok := s.tokenFor(info.AllocID, info.Task)
	now := s.now()
	s.lock.Lock()
	defer s.lock.Unlock()
	if t, ok := s.tokens[tok]; ok {
		t.info = info
		t.expires = now.Add(s.ttl)
		return tok
	}
	s.tokens[tok] = &token{
		info:     info,
		accessor: accessorOf(tok),
		created:  now,
		expires:  now.Add(s.ttl),
		limiter:  rate.NewLimiter(s.limit, s.burst),
	}
	return tok
}

// lookup returns the live token.
func (s *tokenStore) lookup(tok string) (*token, error) {
	if _, _, ok := s.parse(tok); !ok {
		return nil, errTokenInvalid
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	t, ok := s.tokens[tok]
	if !ok {
		return nil, errTokenExpired
	}
	if t.renewed {
		// Renewed by the client while the task runs.
		t.expires = s.now().Add(s.ttl)
	}
	if !s.now().Before(t.expires) {
		delete(s.tokens, tok)
		return nil, errTokenExpired
	}
	return t, nil
}

// renewSelf extends a live token by the increment, at most its TTL, and
// returns its remaining time to live.
func (s *tokenStore) renewSelf(tok string, increment time.Duration) (*token, time.Duration, error) {
	t, err := s.lookup(tok)
	if err != nil {
		return nil, 0, err
	}
	if increment <= 0 || increment > s.ttl {
		increment = s.ttl
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if exp := s.now().Add(increment); exp.After(t.expires) {
		t.expires = exp
	}
	return t, t.expires.Sub(s.now()), nil
}

// track marks a token as renewed by the client; adopt registers a token
// recovered after a restart first.
func (s *tokenStore) track(tok string, adopt func(allocID, task string) (*TaskInfo, bool)) error {
	allocID, task, ok := s.parse(tok)
	if !ok {
		return errTokenInvalid
	}
	s.lock.Lock()
	t, live := s.tokens[tok]
	if live {
		t.renewed = true
		s.lock.Unlock()
		return nil
	}
	s.lock.Unlock()

	info, ok := adopt(allocID, task)
	if !ok {
		return errTokenExpired
	}
	s.derive(info)
	s.lock.Lock()
	defer s.lock.Unlock()
	if t, ok := s.tokens[tok]; ok {
		t.renewed = true
	}
	return nil
}

// revoke removes a token.
func (s *tokenStore) revoke(tok string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.tokens, tok)
}

// len returns the number of live tokens.
func (s *tokenStore) len() int {
	s.lock.Lock()
	defer s.lock.Unlock()
	return len(s.tokens)
}
