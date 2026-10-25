// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	mrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/client/taskenv"
	"github.com/hashicorp/nomad/helper/users"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The files of the native mTLS hook, in the mtls directory of the task's
// secrets directory, and the environment variables naming them. They match
// the constants of the api/mtls package.
const (
	mtlsDir      = "mtls"
	mtlsCertFile = "cert.pem"
	mtlsKeyFile  = "key.pem"
	mtlsCAFile   = "bundle.pem"

	envMTLSCertFile = "NOMAD_MTLS_CERT_FILE"
	envMTLSKeyFile  = "NOMAD_MTLS_KEY_FILE"
	envMTLSCAFile   = "NOMAD_MTLS_CA_FILE"
	envMTLSSPIFFEID = "NOMAD_MTLS_SPIFFE_ID"
)

var (
	// mtlsRetryMin and mtlsRetryMax bound the backoff between failed
	// requests.
	mtlsRetryMin = time.Second
	mtlsRetryMax = 30 * time.Second

	// mtlsRootsWait is the maximum duration of a blocking query on the
	// trust bundle.
	mtlsRootsWait = 5 * time.Minute
)

// mtlsHook gets a short-lived workload certificate for tasks of task groups
// with an mtls service, writes it with its key and the trust bundle into the
// secrets directory, renews it after two thirds of its lifetime, and
// re-issues it when the CA of the region rotates. The key pair is generated
// here; only the CSR is sent to the servers.
type mtlsHook struct {
	alloc    *structs.Allocation
	taskName string
	taskUser string
	rpc      config.RPCer
	region   string
	nodeKey  string
	events   eventEmitter
	logger   log.Logger

	lock    sync.Mutex
	dir     string
	cancel  context.CancelFunc
	cert    *mtlsIssued
	rootIdx uint64
}

// eventEmitter is the part of the task runner the hook uses.
type eventEmitter interface {
	EmitEvent(*structs.TaskEvent)
}

// mtlsIssued describes the current certificate.
type mtlsIssued struct {
	rootID    string
	spiffeID  string
	notBefore time.Time
	notAfter  time.Time
}

type mtlsHookConfig struct {
	alloc   *structs.Allocation
	task    *structs.Task
	rpc     config.RPCer
	region  string
	nodeKey string
	events  eventEmitter
	logger  log.Logger
}

func newMTLSHook(c *mtlsHookConfig) *mtlsHook {
	h := &mtlsHook{
		alloc:    c.alloc,
		taskName: c.task.Name,
		taskUser: c.task.User,
		rpc:      c.rpc,
		region:   c.region,
		nodeKey:  c.nodeKey,
		events:   c.events,
	}
	h.logger = c.logger.Named(h.Name())
	return h
}

func (*mtlsHook) Name() string {
	return "mtls"
}

func (h *mtlsHook) Prestart(ctx context.Context, req *interfaces.TaskPrestartRequest, resp *interfaces.TaskPrestartResponse) error {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.dir = filepath.Join(req.TaskDir.SecretsDir, mtlsDir)
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return fmt.Errorf("failed to create the mtls directory: %w", err)
	}

	// A restarted task keeps a certificate that is still valid; the renewal
	// loop is already running.
	if h.cert == nil || !time.Now().Before(h.renewAt(h.cert)) {
		roots, err := h.fetchRoots(ctx, 0, false)
		if err != nil {
			return err
		}
		if err := h.writeBundle(roots); err != nil {
			return err
		}
		h.rootIdx = roots.Index
		if err := h.issueWithRetry(ctx); err != nil {
			return err
		}
	}

	if h.cancel == nil {
		loopCtx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(loopCtx)
	}

	inTask := filepath.Join(req.TaskEnv.EnvMap[taskenv.SecretsDir], mtlsDir)
	resp.Env = map[string]string{
		envMTLSCertFile: filepath.Join(inTask, mtlsCertFile),
		envMTLSKeyFile:  filepath.Join(inTask, mtlsKeyFile),
		envMTLSCAFile:   filepath.Join(inTask, mtlsCAFile),
		envMTLSSPIFFEID: h.cert.spiffeID,
	}
	return nil
}

// Stop ends the renewal loop.
func (h *mtlsHook) Stop(context.Context, *interfaces.TaskStopRequest, *interfaces.TaskStopResponse) error {
	h.stop()
	return nil
}

// Shutdown ends the renewal loop when the client shuts down.
func (h *mtlsHook) Shutdown() {
	h.stop()
}

func (h *mtlsHook) stop() {
	h.lock.Lock()
	defer h.lock.Unlock()
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
}

// renewAt returns when a certificate is renewed: after two thirds of its
// lifetime, a little earlier for a random share of up to 5% of the lifetime,
// so the certificates of many tasks don't renew at once.
func (h *mtlsHook) renewAt(cert *mtlsIssued) time.Time {
	return mtlsRenewTime(cert.notBefore, cert.notAfter, cert.spiffeID+h.alloc.ID+h.taskName)
}

// mtlsRenewTime computes the renewal time of a certificate. The jitter is
// derived from seed, so it is stable across calls.
func mtlsRenewTime(notBefore, notAfter time.Time, seed string) time.Time {
	lifetime := notAfter.Sub(notBefore)
	var sum uint64
	for _, c := range seed {
		sum = sum*31 + uint64(c)
	}
	jitter := time.Duration(float64(lifetime) * 0.05 * float64(sum%1000) / 1000)
	return notBefore.Add(lifetime*2/3 - jitter)
}

// run renews the certificate and follows the trust bundle until stopped.
func (h *mtlsHook) run(ctx context.Context) {
	rotated := make(chan struct{}, 1)
	go h.watchRoots(ctx, rotated)

	for {
		h.lock.Lock()
		wait := time.Until(h.renewAt(h.cert))
		h.lock.Unlock()

		timer := time.NewTimer(max(wait, 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-rotated:
			timer.Stop()
			// Spread the re-issuance of all tasks after a CA rotation.
			delay := time.Duration(mrand.Int63n(int64(5 * time.Second)))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		h.lock.Lock()
		err := h.issueWithRetry(ctx)
		h.lock.Unlock()
		if err != nil && ctx.Err() == nil {
			h.logger.Error("failed to renew the mTLS certificate", "error", err)
			h.events.EmitEvent(structs.NewTaskEvent(structs.TaskHookFailed).
				SetDisplayMessage(fmt.Sprintf("Failed to renew the mTLS certificate: %v", err)))
			// Try again later: the current certificate stays valid
			// for a third of its lifetime.
			select {
			case <-ctx.Done():
				return
			case <-time.After(mtlsRetryMax):
			}
		}
	}
}

// watchRoots follows the trust bundle with blocking queries. It rewrites the
// bundle file on every change and signals when the active CA is no longer
// the one that signed the current certificate.
func (h *mtlsHook) watchRoots(ctx context.Context, rotated chan<- struct{}) {
	backoff := mtlsRetryMin
	for ctx.Err() == nil {
		h.lock.Lock()
		index := h.rootIdx
		h.lock.Unlock()

		roots, err := h.fetchRoots(ctx, index, true)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.logger.Debug("failed to fetch the mTLS trust bundle", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, mtlsRetryMax)
			continue
		}
		backoff = mtlsRetryMin
		if ctx.Err() != nil {
			return
		}

		h.lock.Lock()
		changed := roots.Index != h.rootIdx
		h.rootIdx = roots.Index
		var stale bool
		if changed {
			if err := h.writeBundle(roots); err != nil {
				h.logger.Error("failed to write the mTLS trust bundle", "error", err)
			}
			stale = h.cert != nil && roots.ActiveRootID != "" && roots.ActiveRootID != h.cert.rootID
		}
		h.lock.Unlock()

		if stale {
			h.logger.Info("certificate authority rotated, re-issuing the mTLS certificate", "root_id", roots.ActiveRootID)
			select {
			case rotated <- struct{}{}:
			default:
			}
		}
	}
}

// fetchRoots reads the trust bundle; with blocking set, it waits for a
// change after index.
func (h *mtlsHook) fetchRoots(ctx context.Context, index uint64, blocking bool) (*structs.MTLSRootsResponse, error) {
	backoff := mtlsRetryMin
	for {
		args := &structs.MTLSRootsRequest{
			QueryOptions: structs.QueryOptions{
				Region:     h.region,
				AllowStale: true,
				AuthToken:  h.nodeKey,
			},
		}
		if blocking {
			args.MinQueryIndex = index
			args.MaxQueryTime = mtlsRootsWait
		}
		var reply structs.MTLSRootsResponse
		err := h.rpc.RPC("MTLS.Roots", args, &reply)
		if err == nil {
			if reply.ActiveRootID == "" {
				err = errors.New("the certificate authority of the region is not initialized yet")
			} else {
				return &reply, nil
			}
		}
		if blocking || mtlsPermanentError(err) {
			return nil, err
		}
		h.logger.Debug("waiting for the mTLS trust bundle", "error", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, mtlsRetryMax)
	}
}

// issueWithRetry gets a new certificate, retrying temporary failures until
// the context ends. The caller holds the lock.
func (h *mtlsHook) issueWithRetry(ctx context.Context) error {
	backoff := mtlsRetryMin
	reported := false
	for {
		err := h.issue()
		if err == nil {
			return nil
		}
		if mtlsPermanentError(err) {
			return structs.NewRecoverableError(fmt.Errorf("failed to get an mTLS certificate: %w", err), false)
		}
		h.logger.Warn("failed to get an mTLS certificate, retrying", "error", err)
		if !reported {
			reported = true
			h.events.EmitEvent(structs.NewTaskEvent(structs.TaskSetup).
				SetMessage(fmt.Sprintf("Waiting for an mTLS certificate: %v", err)))
		}
		h.lock.Unlock()
		select {
		case <-ctx.Done():
			h.lock.Lock()
			return ctx.Err()
		case <-time.After(backoff):
		}
		h.lock.Lock()
		backoff = min(2*backoff, mtlsRetryMax)
	}
}

// mtlsPermanentError returns whether retrying a request can't help.
func mtlsPermanentError(err error) bool {
	if structs.IsErrPermissionDenied(err) {
		return true
	}
	if code, _, ok := structs.CodeFromRPCCodedErr(err); ok {
		return code == http.StatusBadRequest || code == http.StatusNotFound || code == http.StatusForbidden
	}
	return false
}

// issue generates a key pair, has the servers sign a certificate for it and
// writes both. The caller holds the lock.
func (h *mtlsHook) issue() error {
	token := h.alloc.SignedIdentities[h.taskName]
	if token == "" {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "the task has no workload identity")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	args := &structs.MTLSSignRequest{
		AllocID:       h.alloc.ID,
		TaskName:      h.taskName,
		IdentityToken: token,
		CSR:           string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		WriteRequest: structs.WriteRequest{
			Region:    h.region,
			AuthToken: h.nodeKey,
		},
	}
	var reply structs.MTLSSignResponse
	if err := h.rpc.RPC("MTLS.SignCertificate", args, &reply); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key is written first: a reader that sees the new certificate
	// finds its key.
	if err := h.writeFile(mtlsKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	if err := h.writeFile(mtlsCertFile, []byte(reply.CertPEM)); err != nil {
		return err
	}
	h.cert = &mtlsIssued{
		rootID:    reply.RootID,
		spiffeID:  reply.SPIFFEID,
		notBefore: reply.NotBefore,
		notAfter:  reply.NotAfter,
	}
	h.logger.Debug("issued mTLS certificate", "spiffe_id", reply.SPIFFEID, "not_after", reply.NotAfter,
		"renew_at", h.renewAt(h.cert).Format(time.RFC3339))
	return nil
}

// writeBundle writes the trust bundle file. The caller holds the lock.
func (h *mtlsHook) writeBundle(roots *structs.MTLSRootsResponse) error {
	var b strings.Builder
	for _, root := range roots.Roots {
		if root == nil || root.RootCert == "" {
			continue
		}
		b.WriteString(strings.TrimSpace(root.RootCert))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return errors.New("the trust bundle is empty")
	}
	return h.writeFile(mtlsCAFile, []byte(b.String()))
}

// writeFile atomically replaces a file of the mtls directory, owned by the
// task user like the workload identity token.
func (h *mtlsHook) writeFile(name string, contents []byte) error {
	path := filepath.Join(h.dir, name)
	tmp := fmt.Sprintf("%s.tmp%d", path, mrand.Int63n(math.MaxInt32))
	if err := users.WriteFileFor(tmp, contents, h.taskUser); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write %s: %w", name, err)
	}
	return nil
}
