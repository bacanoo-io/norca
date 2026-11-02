// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command secretoperator is a sample operator of the dynamic secrets of Norca,
// built on the controller/secrets package. Its target system is a table in
// memory: it shows the contract, and the lab tests run against it.
//
// The role config "renew" = "replace" makes a renewal return new values, as a
// certificate operator does; "renewable" = "false" makes the credentials end
// with their lease. A param "fail" refuses the request.
//
// It prints what happens to standard output, without the passwords:
//
//	issued lease=<id> ref=<ref> task=<job>/<group>/<task>
//	renewed ref=<ref>
//	revoked ref=<ref>
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bacanoo-io/norca/norca/controller/secrets"
)

// backend is the target system: the users that exist.
type backend struct {
	lock    sync.Mutex
	byLease map[string]*secrets.Credential
	config  map[string]map[string]string
	refs    map[string]string
}

func newBackend() *backend {
	return &backend{byLease: map[string]*secrets.Credential{}, config: map[string]map[string]string{}, refs: map[string]string{}}
}

func randomHex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Issue is idempotent by the lease ID.
func (b *backend) Issue(_ context.Context, req *secrets.IssueRequest) (*secrets.Credential, error) {
	if reason := req.Params["fail"]; reason != "" {
		return nil, &secrets.Invalid{Reason: reason}
	}
	b.lock.Lock()
	defer b.lock.Unlock()
	if cred, ok := b.byLease[req.LeaseID]; ok {
		return cred, nil
	}
	user := "v-" + req.Role + "-" + req.LeaseID[:8]
	cred := &secrets.Credential{
		Items: map[string]string{"username": user, "password": randomHex(), "generation": "1"},
		Ref:   user,
	}
	if req.Config["renewable"] == "false" {
		no := false
		cred.Renewable = &no
	}
	b.byLease[req.LeaseID] = cred
	b.config[req.LeaseID] = req.Config
	b.refs[user] = req.LeaseID
	fmt.Printf("issued lease=%s ref=%s task=%s/%s/%s\n", req.LeaseID, user, req.JobID, req.TaskGroup, req.Task)
	return cred, nil
}

// Renew extends a credential, or replaces it with the "replace" config.
func (b *backend) Renew(_ context.Context, req *secrets.RenewRequest) (*secrets.Renewal, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	fmt.Printf("renewed ref=%s\n", req.Ref)
	cred, ok := b.byLease[req.LeaseID]
	if !ok || b.config[req.LeaseID]["renew"] != "replace" {
		return &secrets.Renewal{}, nil
	}
	generation := 0
	_, _ = fmt.Sscan(cred.Items["generation"], &generation)
	cred.Items = map[string]string{"username": cred.Items["username"], "password": randomHex(),
		"generation": fmt.Sprint(generation + 1)}
	return &secrets.Renewal{Items: cred.Items, Expires: time.Now().Add(time.Hour)}, nil
}

// Revoke removes the users of the list; unknown ones are ignored.
func (b *backend) Revoke(_ context.Context, req *secrets.RevokeRequest) error {
	b.lock.Lock()
	defer b.lock.Unlock()
	for ref := range req.Revoked {
		lease, ok := b.refs[ref]
		if !ok {
			continue
		}
		delete(b.refs, ref)
		delete(b.byLease, lease)
		delete(b.config, lease)
		fmt.Printf("revoked ref=%s\n", ref)
	}
	return nil
}

func main() {
	addr := flag.String("addr", ":"+os.Getenv("NORCA_PORT_api"), "address to listen on")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := secrets.Serve(ctx, *addr, secrets.TLSFromEnv(), newBackend()); err != nil {
		log.Fatal(err)
	}
}
