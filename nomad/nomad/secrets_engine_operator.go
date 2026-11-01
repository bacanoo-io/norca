// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/nomad/nomad/secretengine"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
)

const (
	// secretOperatorMaxConcurrent is the default of operator_max_concurrent.
	secretOperatorMaxConcurrent = 10

	// secretOperatorTimeout is the default of operator_timeout.
	secretOperatorTimeout = 10 * time.Second

	// secretOperatorCertTTL is the lifetime of the client certificate of the
	// leader, renewed at half of it.
	secretOperatorCertTTL = time.Hour

	// secretOperatorMaxAnswer bounds the answer of an operator.
	secretOperatorMaxAnswer = 2 * secretengine.MaxOperatorItems

	// secretLeaseIssueFailed is the revocation reason of a lease whose
	// credential could not be issued.
	secretLeaseIssueFailed = "issue-failed"
)

// serverSPIFFEPath is the path of the SPIFFE ID of the leader in its calls to
// operators.
const serverSPIFFEPath = "/norca/server"

// secretOperatorDeadline bounds a request to an operator, with the wait for
// its slot. A task waits for it when it starts.
func (s *Server) secretOperatorDeadline() (context.Context, context.CancelFunc) {
	timeout := s.config.SecretsOperatorTimeout
	if timeout <= 0 {
		timeout = secretOperatorTimeout
	}
	return context.WithTimeout(s.shutdownCtx, timeout)
}

// operatorSlots bounds the calls in flight to the operator of an engine.
type operatorSlots struct {
	lock  sync.Mutex
	slots map[secretEngineKey]chan struct{}
}

// acquire takes a slot of an engine, waiting within ctx, and returns the
// function that gives it back.
func (o *operatorSlots) acquire(ctx context.Context, key secretEngineKey, limit int) (func(), error) {
	if limit <= 0 {
		limit = secretOperatorMaxConcurrent
	}
	o.lock.Lock()
	if o.slots == nil {
		o.slots = map[secretEngineKey]chan struct{}{}
	}
	ch := o.slots[key]
	if ch == nil {
		ch = make(chan struct{}, limit)
		o.slots[key] = ch
	}
	o.lock.Unlock()

	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, structs.NewErrRPCCodedf(http.StatusTooManyRequests,
			"too many credentials are being issued by the operator of engine %q", key.name)
	}
}

// operatorClientCert is the cached client certificate of the leader.
type operatorClientCert struct {
	lock    sync.Mutex
	cert    *tls.Certificate
	renewAt time.Time
}

// secretOperatorClientCert returns the certificate the leader presents to
// operators, issued by the CA of the region, so the trust bundle the operator
// has as a workload verifies it.
func (s *Server) secretOperatorClientCert() (*tls.Certificate, error) {
	c := &s.secrets.operatorCert
	c.lock.Lock()
	defer c.lock.Unlock()
	now := time.Now()
	if c.cert != nil && now.Before(c.renewAt) {
		return c.cert, nil
	}

	roots, err := s.State().MTLSRoots(nil)
	if err != nil {
		return nil, err
	}
	active, _, _ := mtlsSplitRoots(s.Region(), roots)
	if active == nil {
		return nil, errors.New("the CA of the region is not ready")
	}
	signer, err := s.mtls.signer(active)
	if err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "norca-server"}}, key)
	if err != nil {
		return nil, err
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, err
	}
	certPEM, _, err := signWorkloadCert(signer, csr, "spiffe://"+active.TrustDomain+serverSPIFFEPath, nil, secretOperatorCertTTL)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, errors.New("the client certificate is not PEM")
	}
	c.cert = &tls.Certificate{Certificate: [][]byte{block.Bytes}, PrivateKey: key}
	c.renewAt = now.Add(secretOperatorCertTTL / 2)
	return c.cert, nil
}

// secretOperatorTarget is an instance of the service of an operator.
type secretOperatorTarget struct {
	addr     string
	spiffeID string
}

// secretOperatorTargets returns the instances of the service of an engine,
// with the SPIFFE ID each must present.
func (s *Server) secretOperatorTargets(engine *structs.SecretEngine) ([]secretOperatorTarget, []*x509.Certificate, error) {
	store := s.State()
	namespace := engine.Operator.Namespace
	if namespace == "" {
		namespace = engine.Namespace
	}
	iter, err := store.GetServiceRegistrationByName(nil, namespace, engine.Operator.Service)
	if err != nil {
		return nil, nil, err
	}
	roots, err := store.MTLSRoots(nil)
	if err != nil {
		return nil, nil, err
	}
	active, local, _ := mtlsSplitRoots(s.Region(), roots)
	if active == nil {
		return nil, nil, errors.New("the CA of the region is not ready")
	}
	var pool []*x509.Certificate
	for _, root := range append([]*structs.MTLSRoot{active}, local...) {
		if cert, err := parseMTLSRootCert(root); err == nil {
			pool = append(pool, cert)
		}
	}

	var targets []secretOperatorTarget
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		reg := raw.(*structs.ServiceRegistration)
		alloc, err := store.AllocByID(nil, reg.AllocID)
		if err != nil {
			return nil, nil, err
		}
		if alloc == nil || alloc.ClientTerminalStatus() {
			continue
		}
		targets = append(targets, secretOperatorTarget{
			addr:     net.JoinHostPort(reg.Address, strconv.Itoa(reg.Port)),
			spiffeID: structs.MTLSWorkloadSPIFFEID(active.TrustDomain, namespace, reg.JobID, alloc.TaskGroup),
		})
	}
	if len(targets) == 0 {
		return nil, nil, fmt.Errorf("service %q of the operator has no instances", engine.Operator.Service)
	}
	return targets, pool, nil
}

// secretOperatorTLS is the TLS configuration of a call to an instance: the
// CA of the region and the SPIFFE ID of the service decide, not the address.
func (s *Server) secretOperatorTLS(target secretOperatorTarget, roots []*x509.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	for _, root := range roots {
		pool.AddCert(root)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// VerifyConnection below checks the chain and the SPIFFE ID; the
		// certificate of a workload does not name its address.
		InsecureSkipVerify: true,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return s.secretOperatorClientCert()
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the operator presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			intermediates := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return err
			}
			if !slices.ContainsFunc(leaf.URIs, func(u *url.URL) bool { return u.String() == target.spiffeID }) {
				return fmt.Errorf("the operator is not %s", target.spiffeID)
			}
			return nil
		},
	}
}

// secretOperatorCall returns the call of the operator of an engine.
func (s *Server) secretOperatorCall(engine *structs.SecretEngine) secretengine.OperatorCall {
	return func(ctx context.Context, path string, in, out any) error {
		targets, roots, err := s.secretOperatorTargets(engine)
		if err != nil {
			return err
		}
		body, err := json.Marshal(in)
		if err != nil {
			return err
		}
		var lastErr error
		for _, i := range randomOrder(len(targets)) {
			lastErr = s.secretOperatorPost(ctx, targets[i], roots, path, body, out)
			var status operatorStatusError
			if lastErr == nil || errors.As(lastErr, &status) {
				// The operator answered: another instance would say the same.
				return lastErr
			}
		}
		return lastErr
	}
}

// operatorStatusError is an answer of an operator other than 200. Its body is
// left out: it may hold anything.
type operatorStatusError int

func (e operatorStatusError) Error() string {
	return fmt.Sprintf("the operator answered %d", int(e))
}

func (s *Server) secretOperatorPost(ctx context.Context, target secretOperatorTarget, roots []*x509.Certificate,
	path string, body []byte, out any) error {

	transport := &http.Transport{TLSClientConfig: s.secretOperatorTLS(target, roots), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+target.addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return redactOperatorError(err, path)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, secretOperatorMaxAnswer))
		return operatorStatusError(resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, secretOperatorMaxAnswer+1))
	if err != nil {
		return err
	}
	if len(data) > secretOperatorMaxAnswer {
		return errors.New("the answer of the operator is too large")
	}
	if err := json.Unmarshal(data, out); err != nil {
		// The answer holds credentials: only the fact that it is broken.
		return errors.New("the answer of the operator is not valid JSON")
	}
	return nil
}

// redactOperatorError keeps the reason of a failed call without the URL of
// the request.
func redactOperatorError(err error, path string) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("calling %s: %w", path, err)
}

// randomOrder returns a random permutation of the n first numbers.
func randomOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	for i := n - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			break
		}
		k := int(j.Int64())
		order[i], order[k] = order[k], order[i]
	}
	return order
}

// secretLeaseCreateOperator creates a credential at the operator of an
// engine. The lease is written first, with the lease ID as its reference: a
// credential created while the leader goes away can still be revoked by the
// next one. The engine's lock is not held, so that the calls run in parallel
// up to operator_max_concurrent.
func (s *Server) secretLeaseCreateOperator(engine *structs.SecretEngine, role *structs.SecretEngineRole,
	alloc *structs.Allocation, task string, args *structs.SecretLeaseCreateRequest) (*structs.SecretLease, *secretengine.Credential, uint64, error) {

	backend, err := secretengine.For(engine.Type)
	if err != nil {
		return nil, nil, 0, err
	}
	ctx, cancel := s.secretOperatorDeadline()
	defer cancel()
	release, err := s.secrets.operators.acquire(ctx, secretEngineKey{engine.Namespace, engine.Name}, s.config.SecretsOperatorMaxConcurrent)
	if err != nil {
		return nil, nil, 0, err
	}
	defer release()

	params, err := secretLeaseParams(role, alloc, task, args)
	if err != nil {
		return nil, nil, 0, err
	}

	now := time.Now()
	lease := newSecretLease(engine, role, alloc, task, args, now)
	lease.BackendRef = lease.ID
	if _, err := s.secretLeaseApply([]*structs.SecretLease{lease}); err != nil {
		return nil, nil, 0, err
	}

	c, err := backend.Issue(ctx, &secretengine.IssueRequest{
		Engine:        engine,
		Role:          role,
		LeaseID:       lease.ID,
		Params:        params,
		JobID:         alloc.JobID,
		TaskGroup:     alloc.TaskGroup,
		Task:          task,
		AllocID:       alloc.ID,
		MaxExpireTime: time.Unix(0, lease.MaxExpireTime),
		Call:          s.secretOperatorCall(engine),
	})
	if err != nil {
		// The operator may have created it: the revocation reaches it with
		// the lease ID.
		if _, revokeErr := s.secretLeaseApply([]*structs.SecretLease{
			secretLeaseRevoked(lease.Copy(), secretLeaseIssueFailed, time.Now())}); revokeErr != nil {
			s.logger.Warn("failed to revoke the lease of a failed credential", "lease_id", lease.ID, "error", revokeErr)
		}
		// A request the operator refuses stays refused: it is no outage, and
		// must not start the task on its fallback.
		var status operatorStatusError
		if errors.As(err, &status) && status == http.StatusBadRequest {
			return nil, nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"secret engine %q refused the credential", engine.Name)
		}
		return nil, nil, 0, secretBackendError(engine, "creating the credential", err)
	}

	updated := lease.Copy()
	updated.BackendRef = c.BackendRef
	if !c.Expires.IsZero() {
		end := c.Expires.UnixNano()
		updated.MaxExpireTime = min(updated.MaxExpireTime, end)
		updated.ExpireTime = min(updated.ExpireTime, end)
	}
	if c.NotRenewable {
		updated.ExpireTime = updated.MaxExpireTime
	}
	index, err := s.secretLeaseApply([]*structs.SecretLease{updated})
	if err != nil {
		return nil, nil, 0, err
	}
	stored, err := s.State().SecretLeaseByID(nil, lease.ID)
	if err != nil || stored == nil {
		return nil, nil, 0, fmt.Errorf("lease %s not found after its creation: %v", lease.ID, err)
	}
	return stored.Copy(), c, index, nil
}

// secretLeaseParams returns the params of the task's secret block. They come
// from the job, not from the request, and the role decides which keys a job
// may send.
func secretLeaseParams(role *structs.SecretEngineRole, alloc *structs.Allocation, task string,
	args *structs.SecretLeaseCreateRequest) (map[string]string, error) {

	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	if tg == nil {
		return nil, nil
	}
	t := tg.LookupTask(task)
	if t == nil {
		return nil, nil
	}
	for _, s := range t.Secrets {
		if s == nil || s.Name != args.SecretName {
			continue
		}
		for key := range s.Params {
			if !slices.Contains(role.Params, key) {
				return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
					"role %q does not allow the param %q", role.Name, key)
			}
		}
		return s.Params, nil
	}
	return nil, nil
}

// secretLeaseRenewBackend extends the credential of a lease at its backend.
func (s *Server) secretLeaseRenewBackend(lease *structs.SecretLease) (*secretengine.RenewResult, error) {
	backend, err := secretengine.For(lease.EngineType)
	if err != nil {
		return nil, err
	}
	engine, err := s.State().SecretEngineByName(nil, lease.Namespace, lease.Engine)
	if err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, structs.ErrSecretEngineNotFound
	}
	ctx, cancel := s.secretOperatorDeadline()
	defer cancel()
	release, err := s.secrets.operators.acquire(ctx, secretEngineKey{engine.Namespace, engine.Name}, s.config.SecretsOperatorMaxConcurrent)
	if err != nil {
		return nil, err
	}
	defer release()
	result, err := backend.Renew(ctx, &secretengine.RenewRequest{Engine: engine, Lease: lease, Call: s.secretOperatorCall(engine)})
	if err != nil {
		return nil, secretBackendError(engine, "renewing the credential", err)
	}
	return result, nil
}
