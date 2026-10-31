// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretengine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/secretengine/natstest"
	"github.com/hashicorp/nomad/nomad/structs"
)

func natsTestEngine(ns *natstest.Server) (*structs.SecretEngine, *structs.SecretEngineRole) {
	role := &structs.SecretEngineRole{
		Name:   "worker",
		TTL:    time.Hour,
		MaxTTL: 2 * time.Hour,
		NATS: &structs.SecretRoleNATS{
			Publish:        []string{"orders.>"},
			Subscribe:      []string{"orders.>", "_INBOX.>"},
			PublishDeny:    []string{"orders.secret"},
			AllowResponses: true,
		},
	}
	return &structs.SecretEngine{
		Namespace:      "default",
		Name:           "nats",
		Type:           structs.SecretEngineTypeNATS,
		CredentialPath: "nats/admin",
		NATS:           &structs.SecretEngineNATS{Servers: []string{ns.URL}, AccountJWT: ns.AccountJWT},
		Roles:          []*structs.SecretEngineRole{role},
	}, role
}

// natsConnect connects with the credential and collects the asynchronous
// errors (permission violations) of the connection.
func natsConnect(t *testing.T, url string, cred *Credential) (*nats.Conn, chan error, error) {
	t.Helper()
	errs := make(chan error, 16)
	nc, err := nats.Connect(url,
		nats.UserJWTAndSeed(cred.Items[NATSItemJWT], cred.Items[NATSItemSeed]),
		nats.NoReconnect(),
		nats.Timeout(5*time.Second),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			select {
			case errs <- err:
			default:
			}
		}))
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, errs, err
}

func expectAsyncError(t *testing.T, errs chan error, contains string) {
	t.Helper()
	select {
	case err := <-errs:
		must.StrContains(t, strings.ToLower(err.Error()), strings.ToLower(contains))
	case <-time.After(5 * time.Second):
		t.Fatalf("no asynchronous error containing %q", contains)
	}
}

func issue(t *testing.T, b Backend, engine *structs.SecretEngine, role *structs.SecretEngineRole,
	cred structs.SecretItems, maxExpire time.Time) *Credential {
	t.Helper()
	c, err := b.Issue(context.Background(), &IssueRequest{
		Engine: engine, Role: role, Credential: cred, Name: "test",
		IssueTime: time.Now(), MaxExpireTime: maxExpire,
	})
	must.NoError(t, err)
	return c
}

func TestNATS_Validate(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	engine, _ := natsTestEngine(ns)
	b, err := For(structs.SecretEngineTypeNATS)
	must.NoError(t, err)

	must.NoError(t, b.Validate(engine, ns.Credential()))

	canary := "SAcanary-not-a-seed-7f3a9c"
	bad := structs.SecretItems(ns.Credential())
	bad[NATSItemAccountSigningSeed] = canary
	err = b.Validate(engine, bad)
	must.ErrorContains(t, err, NATSItemAccountSigningSeed)
	must.StrNotContains(t, err.Error(), canary)

	bad = structs.SecretItems(ns.Credential())
	bad[NATSItemOperatorSigningSeed] = ns.AccountSigningSeed // an account seed
	must.ErrorContains(t, b.Validate(engine, bad), NATSItemOperatorSigningSeed)

	bad = structs.SecretItems(ns.Credential())
	delete(bad, NATSItemSystemCreds)
	must.ErrorContains(t, b.Validate(engine, bad), NATSItemSystemCreds)

	broken := engine.Copy()
	broken.NATS.AccountJWT = "not-a-jwt"
	must.ErrorContains(t, b.Validate(broken, ns.Credential()), "invalid account JWT")
}

// TestNATS_Credential checks a credential against the embedded server in
// operator mode: it connects and is limited to the role's subjects.
func TestNATS_Credential(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	engine, role := natsTestEngine(ns)
	b := &NATS{}
	cred := structs.SecretItems(ns.Credential())

	c := issue(t, b, engine, role, cred, time.Now().Add(time.Hour))
	must.Eq(t, NATSDefaultFile, c.File)
	must.Eq(t, NATSItemCreds, c.FileItem)
	must.StrContains(t, c.Items[NATSItemCreds], "BEGIN NATS USER JWT")
	must.StrContains(t, c.Items[NATSItemCreds], c.Items[NATSItemSeed])
	uc, err := jwt.DecodeUserClaims(c.Items[NATSItemJWT])
	must.NoError(t, err)
	must.Eq(t, c.BackendRef, uc.Subject)
	must.Eq(t, ns.AccountKey, uc.IssuerAccount)
	must.Eq(t, c.SigningKey, uc.Issuer)

	// Before the engine's signing key is pushed, the account does not
	// accept the user.
	_, _, err = natsConnect(t, ns.URL, c)
	must.Error(t, err)

	must.NoError(t, b.Sync(context.Background(), &SyncRequest{Engine: engine, Credential: cred}))

	nc, errs, err := natsConnect(t, ns.URL, c)
	must.NoError(t, err)
	sub, err := nc.SubscribeSync("orders.new")
	must.NoError(t, err)
	must.NoError(t, nc.Publish("orders.new", []byte("hello")))
	msg, err := sub.NextMsg(5 * time.Second)
	must.NoError(t, err)
	must.Eq(t, "hello", string(msg.Data))

	// Outside of the role's subjects.
	must.NoError(t, nc.Publish("payments.new", []byte("x")))
	expectAsyncError(t, errs, "permissions violation")
	must.NoError(t, nc.Publish("orders.secret", []byte("x")))
	expectAsyncError(t, errs, "permissions violation")
	_, err = nc.SubscribeSync("payments.>")
	must.NoError(t, err)
	expectAsyncError(t, errs, "permissions violation")
}

// TestNATS_RevokedAndExpired checks that a revoked credential is
// disconnected and refused, and that an expired one is refused.
func TestNATS_RevokedAndExpired(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	engine, role := natsTestEngine(ns)
	b := &NATS{}
	cred := structs.SecretItems(ns.Credential())
	must.NoError(t, b.Sync(context.Background(), &SyncRequest{Engine: engine, Credential: cred}))

	revoked := issue(t, b, engine, role, cred, time.Now().Add(time.Hour))
	kept := issue(t, b, engine, role, cred, time.Now().Add(time.Hour))
	expiring := issue(t, b, engine, role, cred, time.Now().Add(time.Second))

	nc, _, err := natsConnect(t, ns.URL, revoked)
	must.NoError(t, err)
	_, _, err = natsConnect(t, ns.URL, kept)
	must.NoError(t, err)

	must.NoError(t, b.Sync(context.Background(), &SyncRequest{
		Engine: engine, Credential: cred,
		Revoked: map[string]time.Time{revoked.BackendRef: time.Now()},
	}))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(nc.IsClosed),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	_, _, err = natsConnect(t, ns.URL, revoked)
	must.Error(t, err)
	_, _, err = natsConnect(t, ns.URL, kept)
	must.NoError(t, err)

	// The credential expires with its max_ttl (JWT expiry in seconds).
	time.Sleep(2100 * time.Millisecond)
	_, _, err = natsConnect(t, ns.URL, expiring)
	must.Error(t, err)
}

// TestNATS_RotateRoot replaces the account signing key: credentials of the
// old key keep working while it is retired, and are refused once it is
// removed.
func TestNATS_RotateRoot(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	engine, role := natsTestEngine(ns)
	b := &NATS{}
	cred := structs.SecretItems(ns.Credential())
	must.NoError(t, b.Sync(context.Background(), &SyncRequest{Engine: engine, Credential: cred}))
	old := issue(t, b, engine, role, cred, time.Now().Add(time.Hour))

	res, err := b.RotateRoot(context.Background(), &SyncRequest{Engine: engine, Credential: cred})
	must.NoError(t, err)
	must.Eq(t, old.SigningKey, res.OldKey)
	must.NotEq(t, res.OldKey, res.NewKey)
	must.NotEq(t, cred[NATSItemAccountSigningSeed], res.Credential[NATSItemAccountSigningSeed])
	must.Eq(t, cred[NATSItemSystemCreds], res.Credential[NATSItemSystemCreds])

	engine.State = &structs.SecretEngineState{RetiredKeys: []string{res.OldKey}}
	fresh := issue(t, b, engine, role, res.Credential, time.Now().Add(time.Hour))
	must.Eq(t, res.NewKey, fresh.SigningKey)
	oldConn, _, err := natsConnect(t, ns.URL, old)
	must.NoError(t, err)
	_, _, err = natsConnect(t, ns.URL, fresh)
	must.NoError(t, err)

	// The old key is removed once its leases have ended.
	engine.State = &structs.SecretEngineState{RemovedKeys: []string{res.OldKey}}
	must.NoError(t, b.Sync(context.Background(), &SyncRequest{Engine: engine, Credential: res.Credential}))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(oldConn.IsClosed),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))
	_, _, err = natsConnect(t, ns.URL, old)
	must.Error(t, err)
	_, _, err = natsConnect(t, ns.URL, fresh)
	must.NoError(t, err)
}

// TestNATS_PushErrors checks the errors of a push: an unreachable server
// and a server refusing the update. They never contain the credential.
func TestNATS_PushErrors(t *testing.T) {
	ci.Parallel(t)
	ns := natstest.Start(t)
	engine, _ := natsTestEngine(ns)
	b := &NATS{}
	cred := structs.SecretItems(ns.Credential())

	// An account JWT signed by a key the operator does not trust.
	other := natstest.Start(t)
	bad := cred.Copy()
	bad[NATSItemOperatorSigningSeed] = other.OperatorSigningSeed
	err := b.Sync(context.Background(), &SyncRequest{Engine: engine, Credential: bad})
	must.Error(t, err)
	for _, v := range bad {
		must.StrNotContains(t, err.Error(), v)
	}

	down := engine.Copy()
	down.NATS.Servers = []string{"nats://127.0.0.1:1"}
	err = b.Sync(context.Background(), &SyncRequest{Engine: down, Credential: cred})
	must.Error(t, err)
	must.True(t, !errors.Is(err, context.Canceled))
}
