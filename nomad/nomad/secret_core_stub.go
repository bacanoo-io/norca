// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

// TEMPORARY: this file is removed when phase 1 of the secret store (the
// seal) merges. Phase 1 owns the lifecycle of the uBixVault core (creation,
// initialization, unseal, seal) and provides, in secret_store.go:
//
//	func (s *Server) secretCore() (*core.Core, error)
//
// returning the active, unsealed core on the leader, or an error wrapping
// structs.ErrSecretStoreSealed. Until then the accessor below returns the
// core a test registered with setSecretCoreForTest, and a sealed error
// otherwise, so the store compiles and behaves as on a sealed cluster.

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	testing "github.com/mitchellh/go-testing-interface"
)

// errSecretCoreSealedStub stands in for structs.ErrSecretStoreSealed of
// phase 1. Nothing outside this file refers to it.
var errSecretCoreSealedStub = errors.New("secret store is sealed")

var secretCoreStubs = struct {
	sync.Mutex
	cores map[*Server]*core.Core
}{cores: map[*Server]*core.Core{}}

// setSecretCoreForTest registers the core a test created over the server's
// secret store backend; nil removes it.
func setSecretCoreForTest(s *Server, c *core.Core) {
	secretCoreStubs.Lock()
	defer secretCoreStubs.Unlock()
	if c == nil {
		delete(secretCoreStubs.cores, s)
		return
	}
	secretCoreStubs.cores[s] = c
}

// secretCore returns the active, unsealed core of this server.
func (s *Server) secretCore() (*core.Core, error) {
	secretCoreStubs.Lock()
	c := secretCoreStubs.cores[s]
	secretCoreStubs.Unlock()
	if c == nil || !c.Active() || c.Barrier().Sealed() {
		return nil, errSecretCoreSealedStub
	}
	return c, nil
}

// TestSecretCore initializes and unseals a uBixVault core over the secret
// store backend of a single test server (which must become the leader) and
// registers it with setSecretCoreForTest, so the server's secret store is
// usable. It returns a function that removes and stops the core. It is used
// by the tests of other packages; phase 1 replaces it with its own unseal
// helper.
func TestSecretCore(t testing.T, s *Server) func() {
	ctx, cancel := context.WithCancel(context.Background())
	c := core.New(s.secretStore, core.WithHA(core.HAConfig{Backend: s.secretStore, HolderID: s.config.NodeID}))
	init, err := c.Initialize(ctx, core.InitConfig{SecretShares: 3, SecretThreshold: 2})
	if err != nil {
		cancel()
		t.Fatalf("secret store initialization failed: %v", err)
	}
	go c.RunHA(ctx, func(string, ...any) {})
	for _, share := range init.Keys[:2] {
		if _, err := c.Unseal(ctx, share); err != nil {
			cancel()
			t.Fatalf("secret store unseal failed: %v", err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for !c.Active() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("secret store core did not become active")
		}
		time.Sleep(20 * time.Millisecond)
	}
	setSecretCoreForTest(s, c)
	return func() {
		setSecretCoreForTest(s, nil)
		cancel()
	}
}
