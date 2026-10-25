// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// fakeMTLSServer implements the MTLS RPCs with an in-memory CA.
type fakeMTLSServer struct {
	t        *testing.T
	lifetime time.Duration
	signErr  error

	mu      sync.Mutex
	changed chan struct{}
	index   uint64
	active  string
	cas     map[string]*fakeCA
	signed  int
	lastCSR *x509.CertificateRequest
}

type fakeCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newFakeMTLSServer(t *testing.T, lifetime time.Duration) *fakeMTLSServer {
	f := &fakeMTLSServer{t: t, lifetime: lifetime, cas: map[string]*fakeCA{}, changed: make(chan struct{})}
	f.rotate("root-1")
	return f
}

func (f *fakeMTLSServer) rotate(id string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(f.t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), URIs: []*url.URL{{Scheme: "spiffe", Host: "global.nomad"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	must.NoError(f.t, err)
	cert, _ := x509.ParseCertificate(der)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.cas[id] = &fakeCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	f.active = id
	f.index++
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeMTLSServer) signCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signed
}

func (f *fakeMTLSServer) RPC(method string, args, reply any) error {
	switch method {
	case "MTLS.Roots":
		req := args.(*structs.MTLSRootsRequest)
		f.mu.Lock()
		if req.MinQueryIndex >= f.index {
			ch := f.changed
			f.mu.Unlock()
			select {
			case <-ch:
			case <-time.After(200 * time.Millisecond):
			}
			f.mu.Lock()
		}
		defer f.mu.Unlock()
		resp := reply.(*structs.MTLSRootsResponse)
		resp.ActiveRootID = f.active
		resp.TrustDomain = "global.nomad"
		resp.Index = f.index
		resp.Roots = nil
		for id, ca := range f.cas {
			resp.Roots = append(resp.Roots, &structs.MTLSRoot{ID: id, RootCert: ca.pem, Active: id == f.active})
		}
		return nil

	case "MTLS.SignCertificate":
		req := args.(*structs.MTLSSignRequest)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.signErr != nil {
			return f.signErr
		}
		if req.IdentityToken == "" || req.AuthToken != "node-secret" {
			return errors.New("bad credentials")
		}
		block, _ := pem.Decode([]byte(req.CSR))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return err
		}
		f.lastCSR = csr
		ca := f.cas[f.active]
		id := "spiffe://global.nomad/ns/default/job/example/group/web"
		u, _ := url.Parse(id)
		now := time.Now()
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(now.UnixNano()), URIs: []*url.URL{u}, DNSNames: []string{"api.service.nomad"},
			NotBefore: now, NotAfter: now.Add(f.lifetime),
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
		if err != nil {
			return err
		}
		f.signed++
		resp := reply.(*structs.MTLSSignResponse)
		resp.CertPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		resp.SPIFFEID = id
		resp.RootID = f.active
		resp.NotBefore, resp.NotAfter = tmpl.NotBefore, tmpl.NotAfter
		return nil
	}
	return errors.New("unknown method " + method)
}

func newTestMTLSHook(t *testing.T, rpc *fakeMTLSServer) (*mtlsHook, *interfaces.TaskPrestartRequest) {
	alloc := mock.Alloc()
	alloc.SignedIdentities = map[string]string{"web": "jwt"}
	h := newMTLSHook(&mtlsHookConfig{
		alloc:   alloc,
		task:    alloc.Job.TaskGroups[0].Tasks[0],
		rpc:     rpc,
		region:  "global",
		nodeKey: "node-secret",
		events:  &mockEmitter{},
		logger:  testlog.HCLogger(t),
	})
	secrets := t.TempDir()
	req := &interfaces.TaskPrestartRequest{
		TaskDir: &allocdir.TaskDir{SecretsDir: secrets},
		TaskEnv: &taskenv.TaskEnv{EnvMap: map[string]string{taskenv.SecretsDir: "/secrets"}},
	}
	return h, req
}

func TestMTLSHook_IssueRenewRotate(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, 3*time.Second)
	h, req := newTestMTLSHook(t, rpc)
	resp := &interfaces.TaskPrestartResponse{}
	must.NoError(t, h.Prestart(context.Background(), req, resp))
	defer h.Stop(context.Background(), nil, nil)

	dir := filepath.Join(req.TaskDir.SecretsDir, "mtls")
	must.Eq(t, filepath.Join("/secrets", "mtls", "cert.pem"), resp.Env["NOMAD_MTLS_CERT_FILE"])
	must.Eq(t, filepath.Join("/secrets", "mtls", "key.pem"), resp.Env["NOMAD_MTLS_KEY_FILE"])
	must.Eq(t, filepath.Join("/secrets", "mtls", "bundle.pem"), resp.Env["NOMAD_MTLS_CA_FILE"])
	must.Eq(t, "spiffe://global.nomad/ns/default/job/example/group/web", resp.Env["NOMAD_MTLS_SPIFFE_ID"])

	// The key was generated locally: it matches the certificate, and the
	// CSR carried only its public key.
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	must.NoError(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	must.NoError(t, err)
	bundle, err := os.ReadFile(filepath.Join(dir, "bundle.pem"))
	must.NoError(t, err)
	pool := x509.NewCertPool()
	must.True(t, pool.AppendCertsFromPEM(bundle))
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	must.NoError(t, err)
	must.Eq(t, 1, rpc.signCount())

	// Renewed after two thirds of the lifetime.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return rpc.signCount() >= 2 }),
		wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond)))
	renewed, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	must.NoError(t, err)
	must.NotEq(t, string(pair.Certificate[0]), string(renewed.Certificate[0]))

	// A CA rotation rewrites the bundle and re-issues the certificate.
	rpc.mu.Lock()
	rpc.lifetime = time.Hour
	rpc.mu.Unlock()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return rpc.signCount() >= 3 }),
		wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond)))
	before := rpc.signCount()
	rpc.rotate("root-2")
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			data, err := os.ReadFile(filepath.Join(dir, "bundle.pem"))
			return err == nil && strings.Count(string(data), "BEGIN CERTIFICATE") == 2 && rpc.signCount() > before
		}),
		wait.Timeout(15*time.Second), wait.Gap(100*time.Millisecond)))
	h.lock.Lock()
	must.Eq(t, "root-2", h.cert.rootID)
	h.lock.Unlock()

	// A restart keeps the valid certificate.
	count := rpc.signCount()
	must.NoError(t, h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	must.Eq(t, count, rpc.signCount())

	// Stopped: no more renewals.
	must.NoError(t, h.Stop(context.Background(), nil, nil))
	h.lock.Lock()
	must.Nil(t, h.cancel)
	h.lock.Unlock()
}

func TestMTLSHook_PermanentError(t *testing.T) {
	ci.Parallel(t)

	rpc := newFakeMTLSServer(t, time.Hour)
	rpc.signErr = structs.NewErrRPCCodedf(http.StatusBadRequest, "task group %q has no service with an mtls block", "web")
	h, req := newTestMTLSHook(t, rpc)
	err := h.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{})
	must.ErrorContains(t, err, "no service with an mtls block")
	must.False(t, structs.IsRecoverable(err))

	// Temporary errors are retried until the context ends.
	rpc.signErr = errors.New("no servers")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = h.Prestart(ctx, req, &interfaces.TaskPrestartResponse{})
	must.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestMTLSRenewTime(t *testing.T) {
	ci.Parallel(t)

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	for _, seed := range []string{"a", "b", "some-alloc-web"} {
		at := mtlsRenewTime(start, end, seed)
		must.False(t, at.After(start.Add(16*time.Hour)))
		must.True(t, at.After(start.Add(16*time.Hour-72*time.Minute)))
		must.Eq(t, at, mtlsRenewTime(start, end, seed))
	}
}
