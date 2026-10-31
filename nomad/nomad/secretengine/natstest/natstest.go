// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package natstest runs an embedded NATS server in operator mode for the
// tests of the NATS backend of the dynamic secrets engine: an operator with a
// signing key, a system account with a user, and an application account,
// served by the memory resolver.
package natstest

import (
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"
)

// Server is an embedded NATS server in operator mode.
type Server struct {
	*server.Server

	// URL is the client URL of the server.
	URL string

	// AccountKey and AccountJWT are the application account's public key
	// and its JWT as issued by the operator, without signing keys.
	AccountKey string
	AccountJWT string

	// AccountSigningSeed, OperatorSigningSeed and SystemCreds are the items
	// of a nats engine's admin credential.
	AccountSigningSeed  string
	OperatorSigningSeed string
	SystemCreds         string
}

// Credential returns the admin credential items of a nats engine.
func (s *Server) Credential() map[string]string {
	return map[string]string{
		"account_signing_seed":  s.AccountSigningSeed,
		"operator_signing_seed": s.OperatorSigningSeed,
		"system_creds":          s.SystemCreds,
	}
}

// Start starts the server; it is shut down at the end of the test.
func Start(t testing.TB) *Server {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	operator, err := nkeys.CreateOperator()
	must(err)
	operatorKey, err := operator.PublicKey()
	must(err)
	opSigner, err := nkeys.CreateOperator()
	must(err)
	opSignerKey, err := opSigner.PublicKey()
	must(err)
	opSignerSeed, err := opSigner.Seed()
	must(err)

	sys, err := nkeys.CreateAccount()
	must(err)
	sysKey, err := sys.PublicKey()
	must(err)
	acc, err := nkeys.CreateAccount()
	must(err)
	accKey, err := acc.PublicKey()
	must(err)
	accSigner, err := nkeys.CreateAccount()
	must(err)
	accSignerSeed, err := accSigner.Seed()
	must(err)

	oc := jwt.NewOperatorClaims(operatorKey)
	oc.Name = "norca-test"
	oc.SigningKeys.Add(opSignerKey)
	oc.SystemAccount = sysKey
	_, err = oc.Encode(operator)
	must(err)

	sc := jwt.NewAccountClaims(sysKey)
	sc.Name = "SYS"
	sysJWT, err := sc.Encode(opSigner)
	must(err)
	ac := jwt.NewAccountClaims(accKey)
	ac.Name = "APP"
	accJWT, err := ac.Encode(opSigner)
	must(err)

	sysUser, err := nkeys.CreateUser()
	must(err)
	sysUserKey, err := sysUser.PublicKey()
	must(err)
	sysUserSeed, err := sysUser.Seed()
	must(err)
	uc := jwt.NewUserClaims(sysUserKey)
	uc.Name = "sys"
	sysUserJWT, err := uc.Encode(sys)
	must(err)
	creds, err := jwt.FormatUserConfig(sysUserJWT, sysUserSeed)
	must(err)

	resolver := &server.MemAccResolver{}
	must(resolver.Store(sysKey, sysJWT))
	must(resolver.Store(accKey, accJWT))

	opts := &server.Options{
		Host:             "127.0.0.1",
		Port:             -1,
		NoLog:            true,
		NoSigs:           true,
		TrustedOperators: []*jwt.OperatorClaims{oc},
		SystemAccount:    sysKey,
		AccountResolver:  resolver,
	}
	srv, err := server.NewServer(opts)
	must(err)
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		srv.Shutdown()
		t.Fatal("NATS server did not start")
	}
	t.Cleanup(srv.Shutdown)

	// The memory resolver applies account updates to loaded accounts only.
	if _, err := srv.LookupAccount(accKey); err != nil {
		t.Fatal(err)
	}

	return &Server{
		Server:              srv,
		URL:                 srv.ClientURL(),
		AccountKey:          accKey,
		AccountJWT:          accJWT,
		AccountSigningSeed:  string(accSignerSeed),
		OperatorSigningSeed: string(opSignerSeed),
		SystemCreds:         string(creds),
	}
}
