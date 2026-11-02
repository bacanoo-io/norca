// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/secretengine"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	memdb "github.com/hashicorp/go-memdb"
	"github.com/shoenig/test/must"
)

// mockOperatorPassword is the password of the credentials of mockOperator.
const mockOperatorPassword = "c4n4ry-0f-th3-m0ck-0p3r4t0r"

// mockOperator is an operator: an HTTPS server with a workload certificate of
// the region that keeps what it was called with.
type mockOperator struct {
	srv   *httptest.Server
	alloc *structs.Allocation
	reg   *structs.ServiceRegistration
	lock  sync.Mutex

	issued  []*secretengine.OperatorIssueRequest
	renewed []*secretengine.OperatorRenewRequest
	revoked map[string]time.Time
	clients []string

	status    atomic.Int32
	expires   time.Time
	renewable *bool
	newItems  structs.SecretItems
	delay     time.Duration
	inFlight  atomic.Int32
	most      atomic.Int32
}

// startMockOperator starts an operator registered as the service "pg". Its
// certificate names the job of the service, or certJob if it is set.
func startMockOperator(t *testing.T, et *engineTest, certJob string) *mockOperator {
	t.Helper()
	root := waitForMTLSRoot(t, et.s)
	signer, err := et.s.mtls.signer(root)
	must.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	must.NoError(t, err)
	csr, err := x509.ParseCertificateRequest(der)
	must.NoError(t, err)
	alloc := mock.Alloc()
	alloc.ClientStatus = structs.AllocClientStatusRunning
	if certJob == "" {
		certJob = alloc.JobID
	}
	certPEM, _, err := signWorkloadCert(signer, csr,
		structs.MTLSWorkloadSPIFFEID(root.TrustDomain, alloc.Namespace, certJob, alloc.TaskGroup), nil, time.Hour)
	must.NoError(t, err)
	block, _ := pem.Decode([]byte(certPEM))
	must.NotNil(t, block)

	op := &mockOperator{revoked: map[string]time.Time{}}
	op.status.Store(http.StatusOK)
	op.srv = httptest.NewUnstartedServer(http.HandlerFunc(op.handle))
	pool := x509.NewCertPool()
	rootCert, err := parseMTLSRootCert(root)
	must.NoError(t, err)
	pool.AddCert(rootCert)
	op.srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{block.Bytes}, PrivateKey: key}},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	op.srv.StartTLS()
	t.Cleanup(op.srv.Close)

	host, port, err := net.SplitHostPort(op.srv.Listener.Addr().String())
	must.NoError(t, err)
	portNum, err := strconv.Atoi(port)
	must.NoError(t, err)
	op.alloc = alloc
	op.reg = &structs.ServiceRegistration{
		ID: "pg-1", ServiceName: "pg", Namespace: alloc.Namespace, NodeID: alloc.NodeID, Datacenter: "dc1",
		JobID: alloc.JobID, AllocID: alloc.ID, Address: host, Port: portNum,
	}
	op.register(t, et.s)
	return op
}

// register puts the service of the operator into the state of a server.
func (op *mockOperator) register(t *testing.T, s *Server) {
	t.Helper()
	store := s.fsm.State()
	index, _ := store.LatestIndex()
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{op.alloc.Copy()}))
	must.NoError(t, store.UpsertServiceRegistrations(structs.MsgTypeTestSetup, index+2, []*structs.ServiceRegistration{op.reg.Copy()}))
}

// isRevoked reports whether the operator got the revocation of a reference.
func (op *mockOperator) isRevoked(ref string) bool {
	op.lock.Lock()
	defer op.lock.Unlock()
	_, ok := op.revoked[ref]
	return ok
}

func (op *mockOperator) handle(w http.ResponseWriter, r *http.Request) {
	running := op.inFlight.Add(1)
	defer op.inFlight.Add(-1)
	for {
		most := op.most.Load()
		if running <= most || op.most.CompareAndSwap(most, running) {
			break
		}
	}
	time.Sleep(op.delay)

	op.lock.Lock()
	defer op.lock.Unlock()
	if len(r.TLS.PeerCertificates) > 0 && len(r.TLS.PeerCertificates[0].URIs) > 0 {
		op.clients = append(op.clients, r.TLS.PeerCertificates[0].URIs[0].String())
	}
	if status := int(op.status.Load()); status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	switch r.URL.Path {
	case secretengine.OperatorPathIssue:
		var req secretengine.OperatorIssueRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		op.issued = append(op.issued, &req)
		_ = json.NewEncoder(w).Encode(&secretengine.OperatorIssueResponse{
			Items: structs.SecretItems{"username": "v-" + req.Task, "password": mockOperatorPassword},
			Ref:   "ref-" + req.LeaseID, Expires: op.expires, Renewable: op.renewable,
		})
	case secretengine.OperatorPathRenew:
		var req secretengine.OperatorRenewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		op.renewed = append(op.renewed, &req)
		_ = json.NewEncoder(w).Encode(&secretengine.OperatorRenewResponse{Expires: op.expires, Items: op.newItems})
	case secretengine.OperatorPathRevoke:
		var req secretengine.OperatorRevokeRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		for ref, at := range req.Revoked {
			op.revoked[ref] = at
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func operatorEngine() *structs.SecretEngine {
	return &structs.SecretEngine{
		Namespace: structs.DefaultNamespace,
		Name:      "pg",
		Type:      structs.SecretEngineTypeOperator,
		Operator:  &structs.SecretEngineOperator{Service: "pg"},
		Roles: []*structs.SecretEngineRole{{
			Name: "worker", TTL: time.Minute, MaxTTL: time.Hour,
			Config: map[string]string{"database": "app"},
		}},
	}
}

func TestSecretEngine_OperatorValidate(t *testing.T) {
	ci.Parallel(t)

	engine := operatorEngine()
	engine.Canonicalize()
	must.NoError(t, engine.Validate())

	noService := operatorEngine()
	noService.Operator = nil
	must.ErrorContains(t, noService.Validate(), "operator block")
}

func TestSecretEngine_OperatorLease(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)

	// The engine can be created before its operator runs.
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)

	stored := startMockOperator(t, et, "")
	_, token := et.alloc(true)

	resp, err := et.create(token)
	must.NoError(t, err)
	must.Eq(t, mockOperatorPassword, resp.Items["password"])
	lease := et.lease(resp.Lease.ID)
	must.Eq(t, "ref-"+lease.ID, lease.BackendRef)
	must.Eq(t, structs.SecretEngineTypeOperator, lease.EngineType)

	stored.lock.Lock()
	must.Len(t, 1, stored.issued)
	must.Eq(t, lease.ID, stored.issued[0].LeaseID)
	must.Eq(t, lease.TTL, stored.issued[0].TTL)
	must.Positive(t, stored.issued[0].TTL)
	must.Eq(t, "app", stored.issued[0].Config["database"])
	must.StrContains(t, stored.clients[0], serverSPIFFEPath)
	stored.lock.Unlock()

	// The revocations reach the operator in one call, by the watcher.
	revoked := secretLeaseRevoked(lease.Copy(), structs.SecretLeaseRevokeOperator, time.Now())
	_, err = et.s.secretLeaseApply([]*structs.SecretLease{revoked})
	must.NoError(t, err)
	w := testLeaseWatcher(et.s, time.Now)
	w.evaluate(context.Background(), memdb.NewWatchSet())
	stored.lock.Lock()
	defer stored.lock.Unlock()
	_, ok := stored.revoked["ref-"+lease.ID]
	must.True(t, ok)
}

// An operator that fails leaves a revoked lease, whose reference is the lease
// ID: it may have created the credential.
func TestSecretEngine_OperatorIssueFails(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	op.status.Store(http.StatusInternalServerError)
	_, token := et.alloc(true)

	_, err = et.create(token)
	must.ErrorContains(t, err, "the operator answered 500")

	iter, err := et.s.fsm.State().SecretLeases(nil)
	must.NoError(t, err)
	lease := iter.Next().(*structs.SecretLease)
	must.True(t, lease.Revoked)
	must.Eq(t, secretLeaseIssueFailed, lease.RevokeReason)
	must.Eq(t, lease.ID, lease.BackendRef)
}

// A certificate that does not belong to the service is refused.
func TestSecretEngine_OperatorWrongIdentity(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "someone-else")
	_, token := et.alloc(true)

	_, err = et.create(token)
	must.Error(t, err)
	must.StrContains(t, err.Error(), "the operator is not")
	op.lock.Lock()
	defer op.lock.Unlock()
	must.Len(t, 0, op.issued)
}

// The lease ends with the credential, and a renewal reaches the operator.
func TestSecretEngine_OperatorRenew(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	op.expires = time.Now().Add(30 * time.Minute)
	_, token := et.alloc(true)

	resp, err := et.create(token)
	must.NoError(t, err)
	lease := et.lease(resp.Lease.ID)
	must.Eq(t, op.expires.UnixNano(), lease.MaxExpireTime)

	var renew structs.SecretLeaseWriteResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{
		LeaseID:      lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &renew))
	op.lock.Lock()
	must.Len(t, 1, op.renewed)
	must.Eq(t, "ref-"+lease.ID, op.renewed[0].Ref)
	op.lock.Unlock()

	op.status.Store(http.StatusBadGateway)
	err = et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{
		LeaseID:      lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &renew)
	must.ErrorContains(t, err, "renewing the credential failed")
	must.False(t, et.lease(lease.ID).Revoked)
}

// The calls in flight stay within operator_max_concurrent, and they do run in
// parallel: the engine's lock is not held.
func TestSecretEngine_OperatorConcurrency(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, func(c *Config) { c.SecretsOperatorMaxConcurrent = 2 })
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	op.delay = 100 * time.Millisecond

	tokens := make([]string, 6)
	for i := range tokens {
		_, tokens[i] = et.alloc(true)
	}
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := et.create(token)
			must.NoError(t, err)
		}()
	}
	wg.Wait()
	must.Eq(t, int32(2), op.most.Load())
}

// The params come from the job, and the role decides which keys it may send.
func TestSecretEngine_OperatorParams(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	engine := operatorEngine()
	engine.Roles[0].Params = []string{"common_name"}
	_, err := et.upsert(et.root.SecretID, engine)
	must.NoError(t, err)
	op := startMockOperator(t, et, "")

	alloc, token := et.alloc(true)
	alloc.Job.TaskGroups[0].Tasks[0].Secrets[0].Params = map[string]string{"common_name": "api.internal"}
	resp, err := et.create(token)
	must.NoError(t, err)
	op.lock.Lock()
	must.Eq(t, "api.internal", op.issued[0].Params["common_name"])
	op.lock.Unlock()
	must.NotNil(t, resp.Lease)

	alloc, token = et.alloc(true)
	alloc.Job.TaskGroups[0].Tasks[0].Secrets[0].Params = map[string]string{"ttl": "1000h"}
	_, err = et.create(token)
	must.ErrorContains(t, err, `does not allow the param "ttl"`)
}

// A credential that can't be extended is replaced: the renewal returns its new
// values, and they are not stored.
func TestSecretEngine_OperatorRenewReplaces(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	_, token := et.alloc(true)
	resp, err := et.create(token)
	must.NoError(t, err)

	op.lock.Lock()
	op.newItems = structs.SecretItems{"cert": "CERT-2"}
	op.expires = time.Now().Add(2 * time.Hour)
	op.lock.Unlock()
	var renew structs.SecretLeaseWriteResponse
	must.NoError(t, et.s.RPC(structs.SecretLeaseRenewRPCMethod, &structs.SecretLeaseRenewRequest{
		LeaseID:      resp.Lease.ID,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: structs.DefaultNamespace, AuthToken: token},
	}, &renew))
	must.Eq(t, "CERT-2", renew.Items["cert"])
	must.Eq(t, op.expires.UnixNano(), renew.Lease.MaxExpireTime)
}

// A lease of a credential that can't be extended ends with it.
func TestSecretEngine_OperatorNotRenewable(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	no := false
	op.renewable = &no
	_, token := et.alloc(true)
	resp, err := et.create(token)
	must.NoError(t, err)
	lease := et.lease(resp.Lease.ID)
	must.Eq(t, lease.MaxExpireTime, lease.ExpireTime)
}

// A slow operator fails within operator_timeout.
func TestSecretEngine_OperatorTimeout(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, func(c *Config) { c.SecretsOperatorTimeout = 100 * time.Millisecond })
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	op.delay = time.Second
	_, token := et.alloc(true)

	start := time.Now()
	_, err = et.create(token)
	must.Error(t, err)
	must.Less(t, time.Second, time.Since(start))
}

// A request the operator refuses is no outage: the status is kept, so the
// client does not start the task on its fallback.
func TestSecretEngine_OperatorRefuses(t *testing.T) {
	ci.Parallel(t)
	et := newEngineTest(t, nil)
	_, err := et.upsert(et.root.SecretID, operatorEngine())
	must.NoError(t, err)
	op := startMockOperator(t, et, "")
	op.status.Store(http.StatusBadRequest)
	_, token := et.alloc(true)

	_, err = et.create(token)
	code, _, ok := structs.CodeFromRPCCodedErr(err)
	must.True(t, ok, must.Sprint(err))
	must.Eq(t, http.StatusBadRequest, code)
}
