// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/api/mtls"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func init() {
	// Test servers create their CA right after the keyring.
	mtlsCAInterval = 200 * time.Millisecond
	mtlsCARetryInterval = 100 * time.Millisecond
}

// waitForMTLSRoot waits for the active CA of the server's region.
func waitForMTLSRoot(t *testing.T, srv *Server) *structs.MTLSRoot {
	t.Helper()
	var active *structs.MTLSRoot
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			roots, err := srv.State().MTLSRoots(nil)
			must.NoError(t, err)
			active, _, _ = mtlsSplitRoots(srv.Region(), roots)
			return active != nil
		}),
		wait.Timeout(20*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	return active
}

// mtlsTestAlloc stores a job with an mtls service and a running allocation
// of it, and returns the allocation and the identity token of its task.
func mtlsTestAlloc(t *testing.T, srv *Server, mutate func(*structs.Job)) (*structs.Allocation, string) {
	t.Helper()
	store := srv.fsm.State()
	node := mock.Node()
	index, err := store.LatestIndex()
	must.NoError(t, err)
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, index+1, node))

	alloc := mock.Alloc()
	job := alloc.Job
	job.TaskGroups[0].Services = []*structs.Service{{
		Name:     "api",
		Provider: structs.ServiceProviderNomad,
		MTLS:     &structs.ServiceMTLS{Mode: structs.MTLSModeApp},
	}}
	if mutate != nil {
		mutate(job)
	}
	must.NoError(t, store.UpsertJob(structs.MsgTypeTestSetup, index+2, nil, job))
	alloc.NodeID = node.ID
	alloc.ClientStatus = structs.AllocClientStatusRunning
	token, _, err := srv.encrypter.SignClaims(alloc.ToTaskIdentityClaims(job, "web"))
	must.NoError(t, err)
	alloc.SignedIdentities = map[string]string{"web": token}
	must.NoError(t, store.UpsertAllocs(structs.MsgTypeTestSetup, index+3, []*structs.Allocation{alloc}))
	return alloc, token
}

func newTestCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	must.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func mtlsRoots(t *testing.T, srv *Server) *structs.MTLSRootsResponse {
	t.Helper()
	var reply structs.MTLSRootsResponse
	must.NoError(t, srv.RPC("MTLS.Roots", &structs.MTLSRootsRequest{
		QueryOptions: structs.QueryOptions{Region: srv.Region()}}, &reply))
	return &reply
}

func mtlsBundle(t *testing.T, roots *structs.MTLSRootsResponse) *mtls.Bundle {
	t.Helper()
	var data []byte
	for _, root := range roots.Roots {
		data = append(data, root.RootCert...)
	}
	bundle, err := mtls.ParseBundle(data)
	must.NoError(t, err)
	return bundle
}

func parsePEMCert(t *testing.T, data string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(data))
	must.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	must.NoError(t, err)
	return cert
}

func TestMTLS_CertificateAuthorityAndSigning(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	root := waitForMTLSRoot(t, srv)

	// The CA: public certificate, key encrypted with the active root key.
	keyMeta, err := srv.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	must.Eq(t, keyMeta.KeyID, root.SigningKeyID)
	must.SliceNotEmpty(t, root.SigningKey)
	must.Eq(t, "global.nomad", root.TrustDomain)
	caCert := parsePEMCert(t, root.RootCert)
	must.True(t, caCert.IsCA)
	must.Eq(t, "spiffe://global.nomad", caCert.URIs[0].String())
	inUse, err := srv.State().IsRootKeyMetaInUse(keyMeta.KeyID)
	must.NoError(t, err)
	must.True(t, inUse)

	// The trust bundle never carries key material.
	roots := mtlsRoots(t, srv)
	must.Eq(t, root.ID, roots.ActiveRootID)
	must.Eq(t, "global.nomad", roots.TrustDomain)
	must.Len(t, 1, roots.Roots)
	must.Nil(t, roots.Roots[0].SigningKey)
	must.Eq(t, "", roots.Roots[0].SigningKeyID)

	alloc, token := mtlsTestAlloc(t, srv, nil)
	sign := func(mutate func(*structs.MTLSSignRequest)) (*structs.MTLSSignResponse, error) {
		args := &structs.MTLSSignRequest{
			AllocID:       alloc.ID,
			TaskName:      "web",
			IdentityToken: token,
			CSR:           newTestCSR(t),
			WriteRequest:  structs.WriteRequest{Region: "global"},
		}
		if mutate != nil {
			mutate(args)
		}
		var reply structs.MTLSSignResponse
		err := srv.RPC("MTLS.SignCertificate", args, &reply)
		return &reply, err
	}

	reply, err := sign(nil)
	must.NoError(t, err)
	wantID := "spiffe://global.nomad/ns/default/job/" + alloc.JobID + "/group/web"
	must.Eq(t, wantID, reply.SPIFFEID)
	must.Eq(t, root.ID, reply.RootID)
	cert := parsePEMCert(t, reply.CertPEM)
	must.Eq(t, wantID, cert.URIs[0].String())
	must.SliceContains(t, cert.DNSNames, "api.service.default.nomad")
	must.SliceContains(t, cert.DNSNames, "api.service.nomad")
	must.False(t, cert.IsCA)
	lifetime := cert.NotAfter.Sub(time.Now())
	must.Greater(t, 23*time.Hour, lifetime)
	must.Less(t, 25*time.Hour, lifetime)
	peer, err := mtlsBundle(t, roots).Verify([]*x509.Certificate{cert})
	must.NoError(t, err)
	must.Eq(t, []string{"api"}, peer.Services)
	must.Eq(t, alloc.JobID, peer.ID.Job)

	// The identity must match the allocation and task.
	_, err = sign(func(r *structs.MTLSSignRequest) { r.TaskName = "other" })
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = sign(func(r *structs.MTLSSignRequest) { r.IdentityToken = "not-a-jwt" })
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	_, err = sign(func(r *structs.MTLSSignRequest) { r.CSR = "" })
	must.ErrorContains(t, err, "CSR are required")
	_, err = sign(func(r *structs.MTLSSignRequest) {
		r.CSR = "-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----\n"
	})
	must.ErrorContains(t, err, "CSR")

	// A group without mtls service gets no certificate.
	plain, plainToken := mtlsTestAlloc(t, srv, func(j *structs.Job) { j.TaskGroups[0].Services = nil })
	var plainReply structs.MTLSSignResponse
	err = srv.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
		AllocID: plain.ID, TaskName: "web", IdentityToken: plainToken, CSR: newTestCSR(t),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &plainReply)
	must.ErrorContains(t, err, "no service with an mtls block")

	// A stopped allocation gets no certificate.
	stopped := alloc.Copy()
	stopped.DesiredStatus = structs.AllocDesiredStatusStop
	index, _ := srv.State().LatestIndex()
	must.NoError(t, srv.State().UpsertAllocs(structs.MsgTypeTestSetup, index+1, []*structs.Allocation{stopped}))
	_, err = sign(nil)
	must.ErrorContains(t, err, "terminal")
}

func TestMTLS_SignACL(t *testing.T) {
	ci.Parallel(t)

	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	waitForMTLSRoot(t, srv)

	alloc, token := mtlsTestAlloc(t, srv, nil)
	node, err := srv.State().NodeByID(nil, alloc.NodeID)
	must.NoError(t, err)
	other := mock.Node()
	index, _ := srv.State().LatestIndex()
	must.NoError(t, srv.State().UpsertNode(structs.MsgTypeTestSetup, index+1, other))

	sign := func(authToken string) error {
		var reply structs.MTLSSignResponse
		return srv.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
			AllocID: alloc.ID, TaskName: "web", IdentityToken: token, CSR: newTestCSR(t),
			WriteRequest: structs.WriteRequest{Region: "global", AuthToken: authToken},
		}, &reply)
	}
	// The node running the allocation, and the task itself, may ask.
	must.NoError(t, sign(node.SecretID))
	must.NoError(t, sign(token))
	// Another node or an ACL token may not, even a management token.
	must.EqError(t, sign(other.SecretID), structs.ErrPermissionDenied.Error())
	must.EqError(t, sign(root.SecretID), structs.ErrPermissionDenied.Error())

	// The trust bundle is public.
	var reply structs.MTLSRootsResponse
	must.NoError(t, srv.RPC("MTLS.Roots", &structs.MTLSRootsRequest{
		QueryOptions: structs.QueryOptions{Region: "global"}}, &reply))
	must.Len(t, 1, reply.Roots)

	// Rotation needs a management token.
	var rotate structs.MTLSRotateResponse
	err = srv.RPC("MTLS.RotateCA", &structs.MTLSRotateRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: node.SecretID}}, &rotate)
	must.EqError(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, srv.RPC("MTLS.RotateCA", &structs.MTLSRotateRequest{
		WriteRequest: structs.WriteRequest{Region: "global", AuthToken: root.SecretID}}, &rotate))
	must.NotEq(t, "", rotate.RootID)
}

func TestMTLS_RotationRewrapAndPrune(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	testutil.WaitForLeader(t, srv.RPC)
	first := waitForMTLSRoot(t, srv)
	alloc, token := mtlsTestAlloc(t, srv, nil)

	// Rotate the CA: the old one stays trusted, without its key.
	var rotate structs.MTLSRotateResponse
	must.NoError(t, srv.RPC("MTLS.RotateCA", &structs.MTLSRotateRequest{
		WriteRequest: structs.WriteRequest{Region: "global"}}, &rotate))
	must.NotEq(t, first.ID, rotate.RootID)
	roots := mtlsRoots(t, srv)
	must.Eq(t, rotate.RootID, roots.ActiveRootID)
	must.Len(t, 2, roots.Roots)
	old, err := srv.State().MTLSRootByID(nil, first.ID)
	must.NoError(t, err)
	must.False(t, old.Active)
	must.Nil(t, old.SigningKey)
	must.False(t, old.RotatedOutAt.IsZero())

	var reply structs.MTLSSignResponse
	must.NoError(t, srv.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
		AllocID: alloc.ID, TaskName: "web", IdentityToken: token, CSR: newTestCSR(t),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &reply))
	must.Eq(t, rotate.RootID, reply.RootID)

	// A root key rotation re-encrypts the CA key with the new root key, so
	// the old root key can be garbage collected.
	oldKey, err := srv.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	var keyResp structs.KeyringRotateRootKeyResponse
	must.NoError(t, srv.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{
		WriteRequest: structs.WriteRequest{Region: "global"}}, &keyResp))
	newKey, err := srv.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	must.NotEq(t, oldKey.KeyID, newKey.KeyID)
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			root, err := srv.State().MTLSRootByID(nil, rotate.RootID)
			must.NoError(t, err)
			return root.SigningKeyID == newKey.KeyID
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(50*time.Millisecond),
	))
	inUse, err := srv.State().IsRootKeyMetaInUse(oldKey.KeyID)
	must.NoError(t, err)
	must.False(t, inUse)
	must.NoError(t, srv.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
		AllocID: alloc.ID, TaskName: "web", IdentityToken: token, CSR: newTestCSR(t),
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &reply))

	// A retired root is removed once its certificates have expired, and an
	// expired root of another region too.
	old.RotatedOutAt = time.Now().Add(-48 * time.Hour)
	foreign := &structs.MTLSRoot{ID: "foreign-expired", Region: "other", TrustDomain: "global.nomad",
		RootCert: old.RootCert, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}
	_, _, err = srv.raftApply(structs.MTLSRootsUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.MTLSRootsUpsertRequest{Roots: []*structs.MTLSRoot{old, foreign}})
	must.NoError(t, err)
	must.NoError(t, srv.mtls.reconcile())
	all, err := srv.State().MTLSRoots(nil)
	must.NoError(t, err)
	must.Len(t, 1, all)
	must.Eq(t, rotate.RootID, all[0].ID)
}

func TestMTLS_Federation(t *testing.T) {
	ci.Parallel(t)

	s1, cleanup1 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.Region = "eu"
	})
	defer cleanup1()
	s2, cleanup2 := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
		c.Region = "us"
	})
	defer cleanup2()
	TestJoin(t, s1, s2)
	testutil.WaitForLeader(t, s1.RPC)
	testutil.WaitForLeader(t, s2.RPC)

	r1 := waitForMTLSRoot(t, s1)
	r2 := waitForMTLSRoot(t, s2)
	must.Eq(t, "global.nomad", r1.TrustDomain)
	must.Eq(t, r1.TrustDomain, r2.TrustDomain)

	// Each region trusts the CA of the other.
	hasRoot := func(srv *Server, id string) func() bool {
		return func() bool {
			roots, err := srv.State().MTLSRoots(nil)
			must.NoError(t, err)
			return slices.ContainsFunc(roots, func(r *structs.MTLSRoot) bool { return r.ID == id })
		}
	}
	for _, check := range []func() bool{hasRoot(s1, r2.ID), hasRoot(s2, r1.ID)} {
		must.Wait(t, wait.InitialSuccess(wait.BoolFunc(check), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
	}
	copied, err := s1.State().MTLSRootByID(nil, r2.ID)
	must.NoError(t, err)
	must.Eq(t, "us", copied.Region)
	must.Nil(t, copied.SigningKey)
	must.False(t, copied.Active && copied.Region == s1.Region())

	// A certificate of one region verifies with the bundle of the other.
	alloc, token := mtlsTestAlloc(t, s2, nil)
	var reply structs.MTLSSignResponse
	must.NoError(t, s2.RPC("MTLS.SignCertificate", &structs.MTLSSignRequest{
		AllocID: alloc.ID, TaskName: "web", IdentityToken: token, CSR: newTestCSR(t),
		WriteRequest: structs.WriteRequest{Region: "us"},
	}, &reply))
	roots := mtlsRoots(t, s1)
	must.Eq(t, r1.ID, roots.ActiveRootID)
	must.Eq(t, "eu", roots.Roots[0].Region)
	_, err = mtlsBundle(t, roots).Verify([]*x509.Certificate{parsePEMCert(t, reply.CertPEM)})
	must.NoError(t, err)

	// A rotation in one region reaches the other.
	var rotate structs.MTLSRotateResponse
	must.NoError(t, s2.RPC("MTLS.RotateCA", &structs.MTLSRotateRequest{
		WriteRequest: structs.WriteRequest{Region: "us"}}, &rotate))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(hasRoot(s1, rotate.RootID)), wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond)))
}

func TestMTLS_FederationChanges(t *testing.T) {
	ci.Parallel(t)

	a1 := &structs.MTLSRoot{ID: "a1", Region: "a", RootCert: "pem-a1", Active: true}
	b1 := &structs.MTLSRoot{ID: "b1", Region: "b", RootCert: "pem-b1", Active: true}
	b2 := &structs.MTLSRoot{ID: "b2", Region: "b", RootCert: "pem-b2", Active: true}
	retiredB1 := b1.Copy()
	retiredB1.Active = false

	// Region a unreachable (not fetched): its root stays. b rotated.
	req := mtlsFederationChanges([]*structs.MTLSRoot{a1, b1},
		map[string][]*structs.MTLSRoot{"b": {retiredB1, b2}}, true)
	must.SliceEmpty(t, req.DeleteIDs)
	must.Len(t, 2, req.Roots)

	// b reports only b2: b1 is removed. Unchanged roots are not written.
	req = mtlsFederationChanges([]*structs.MTLSRoot{a1, retiredB1, b2},
		map[string][]*structs.MTLSRoot{"b": {b2}}, true)
	must.Eq(t, []string{"b1"}, req.DeleteIDs)
	must.SliceEmpty(t, req.Roots)

	// Federation disabled: every foreign root goes.
	req = mtlsFederationChanges([]*structs.MTLSRoot{a1, b2}, nil, false)
	must.Eq(t, []string{"a1", "b2"}, req.DeleteIDs)
}

func TestMTLS_JobValidation(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)

	register := func(job *structs.Job) (*structs.JobRegisterResponse, error) {
		var resp structs.JobRegisterResponse
		err := msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}, &resp)
		return &resp, err
	}
	withMTLS := func(mtls *structs.ServiceMTLS) *structs.Job {
		job := mock.Job()
		job.TaskGroups[0].Tasks[0].Services = []*structs.Service{{
			Name: "api", PortLabel: "http", Provider: structs.ServiceProviderNomad, MTLS: mtls,
		}}
		return job
	}

	_, err := register(withMTLS(&structs.ServiceMTLS{Mode: structs.MTLSModeApp}))
	must.NoError(t, err)

	_, err = register(withMTLS(&structs.ServiceMTLS{
		Mode:      structs.MTLSModeSidecar,
		Upstreams: []*structs.ServiceMTLSUpstream{{Name: "db", Service: "postgres", LocalPort: 5432}},
	}))
	must.ErrorContains(t, err, `mtls mode "sidecar" is only supported on group services`)

	job := withMTLS(&structs.ServiceMTLS{})
	job.ID = "web@v2"
	_, err = register(job)
	must.ErrorContains(t, err, "can't be used in a SPIFFE ID")

	job = withMTLS(&structs.ServiceMTLS{})
	job.TaskGroups[0].Tasks[0].Services[0].Name = "${NOMAD_JOB_NAME}-api"
	resp, err := register(job)
	must.NoError(t, err)
	must.StrContains(t, resp.Warnings, "interpolated at runtime")
}

func TestMTLS_SnapshotRestore(t *testing.T) {
	ci.Parallel(t)

	fsm := testFSM(t)
	store := fsm.State()
	root := &structs.MTLSRoot{ID: "r1", Region: "global", TrustDomain: "global.nomad", RootCert: "pem",
		SigningKey: []byte{1, 2, 3}, SigningKeyID: "key", Active: true,
		NotBefore: time.Now().UTC().Truncate(time.Second), NotAfter: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	must.NoError(t, store.UpsertMTLSRoots(structs.MsgTypeTestSetup, 10, []*structs.MTLSRoot{root}, nil))
	intention := &structs.ServiceIntention{Namespace: "default", Destination: "api", SourceNamespace: "default", Source: "web", Action: "allow"}
	must.NoError(t, store.UpsertServiceIntentions(structs.MsgTypeTestSetup, 11, []*structs.ServiceIntention{intention}))

	fsm2 := testSnapshotRestore(t, fsm)
	store2 := fsm2.State()
	got, err := store2.MTLSRootByID(nil, "r1")
	must.NoError(t, err)
	must.NotNil(t, got)
	must.Eq(t, root.SigningKey, got.SigningKey)
	must.True(t, root.NotAfter.Equal(got.NotAfter))
	gotIntention, err := store2.ServiceIntentionByKey(nil, "default", "api", "default", "web")
	must.NoError(t, err)
	must.NotNil(t, gotIntention)
	must.Eq(t, "allow", gotIntention.Action)
}

// TestMTLS_FullRestore checks that a full restore from a backup brings the
// CA of the backup back, with a key the restored keyring decrypts.
func TestMTLS_FullRestore(t *testing.T) {
	ci.Parallel(t)

	ft, cleanup := newFullRestoreTest(t, 0)
	defer cleanup()
	srv := ft.srv
	original := waitForMTLSRoot(t, srv)
	oldKey, err := srv.fsm.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	must.Eq(t, oldKey.KeyID, original.SigningKeyID)
	record := ft.save()

	// After the backup: a new root key, a new CA, and the old root key
	// lost, so only the keyring of the backup can decrypt the old CA key.
	var keyResp structs.KeyringRotateRootKeyResponse
	must.NoError(t, ft.call("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{WriteRequest: ft.write(ft.root)}, &keyResp))
	var rotate structs.MTLSRotateResponse
	must.NoError(t, ft.call("MTLS.RotateCA", &structs.MTLSRotateRequest{WriteRequest: ft.write(ft.root)}, &rotate))
	must.NoError(t, srv.encrypter.RemoveKey(oldKey.KeyID))

	requested, err := ft.request(record.ID, ft.root, "global", false)
	must.NoError(t, err)
	approved, err := ft.approve(requested.Restore.ID, ft.second, ft.identity.String())
	must.NoError(t, err)
	must.Eq(t, structs.BackupRestoreStatusComplete, approved.Restore.Status, must.Sprint(approved.Restore.StatusDescription))

	restored := waitForMTLSRoot(t, srv)
	must.Eq(t, original.ID, restored.ID)
	must.Eq(t, original.RootCert, restored.RootCert)
	// The key decrypts with the restored keyring (re-encrypted with the
	// active root key by the leader, or still with the old one).
	signer, err := srv.mtls.signer(restored)
	must.NoError(t, err)
	must.Eq(t, parsePEMCert(t, original.RootCert).SerialNumber.String(), signer.cert.SerialNumber.String())
	_, err = srv.State().MTLSRootByID(nil, rotate.RootID)
	must.NoError(t, err)
}

func TestMTLS_JobProxyInjection(t *testing.T) {
	ci.Parallel(t)

	srv, cleanup := TestServer(t, func(c *Config) {
		c.NumSchedulers = 0
	})
	defer cleanup()
	codec := rpcClient(t, srv)
	testutil.WaitForLeader(t, srv.RPC)

	register := func(job *structs.Job) error {
		var resp structs.JobRegisterResponse
		return msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
			Job:          job,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}, &resp)
	}

	job := mock.Job()
	tg := job.TaskGroups[0]
	tg.Tasks[0].Services = nil
	tg.Networks = structs.Networks{{Mode: "bridge", ReservedPorts: []structs.Port{{Label: "gw", Value: 8443, To: 8443}}}}
	tg.Services = []*structs.Service{
		{
			Name: "api", PortLabel: "8080", Provider: structs.ServiceProviderNomad,
			MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeSidecar, Upstreams: []*structs.ServiceMTLSUpstream{
				{Name: "db", Service: "postgres", LocalPort: 5432},
				{Name: "remote", Service: "redis", Region: "eu", LocalPort: 6379},
			}},
		},
		{
			Name: "mtls-gateway", PortLabel: "gw", Provider: structs.ServiceProviderNomad,
			MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeGateway},
		},
	}
	must.NoError(t, register(job))

	stored, err := srv.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	stg := stored.TaskGroups[0]
	must.Len(t, 3, stg.Tasks)

	proxy := stg.LookupTask("mtls-proxy-api")
	must.NotNil(t, proxy)
	must.True(t, proxy.Kind.IsMTLSProxy())
	must.Eq(t, "api", proxy.Kind.Value())
	must.NotNil(t, proxy.Lifecycle)
	must.True(t, proxy.Lifecycle.Sidecar)
	must.Eq(t, "${meta.norca.mtls.envoy_image}", proxy.Config["image"])

	gateway := stg.LookupTask("mtls-gateway-mtls-gateway")
	must.NotNil(t, gateway)
	must.True(t, gateway.Kind.IsMTLSGateway())
	must.Nil(t, gateway.Lifecycle)

	var injected *structs.Port
	for i, p := range stg.Networks[0].DynamicPorts {
		if p.Label == "mtls-proxy-api" {
			injected = &stg.Networks[0].DynamicPorts[i]
		}
	}
	must.NotNil(t, injected)
	must.Eq(t, -1, injected.To)

	// registering the job again doesn't inject twice
	must.NoError(t, register(stored.Copy()))
	stored, err = srv.State().JobByID(nil, job.Namespace, job.ID)
	must.NoError(t, err)
	must.Len(t, 3, stored.TaskGroups[0].Tasks)

	// sidecars need bridge networking
	host := job.Copy()
	host.ID = "host"
	host.TaskGroups[0].Networks[0].Mode = "host"
	must.ErrorContains(t, register(host), "requires bridge networking")

	// a task with the kind of the proxy replaces the injected one
	custom := job.Copy()
	custom.ID = "custom"
	custom.TaskGroups[0].Tasks = append(custom.TaskGroups[0].Tasks, &structs.Task{
		Name: "my-envoy", Driver: "docker", Kind: structs.NewTaskKind(structs.MTLSProxyPrefix, "api"),
		Config:    map[string]interface{}{"image": "mirror.local/envoy:v1"},
		Resources: structs.DefaultResources(),
		LogConfig: structs.DefaultLogConfig(),
	})
	must.NoError(t, register(custom))
	stored, err = srv.State().JobByID(nil, custom.Namespace, custom.ID)
	must.NoError(t, err)
	must.Len(t, 3, stored.TaskGroups[0].Tasks)
	must.Nil(t, stored.TaskGroups[0].LookupTask("mtls-proxy-api"))
}
