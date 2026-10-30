// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul-template/signals"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	ti "github.com/hashicorp/nomad/client/allocrunner/taskrunner/interfaces"
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

	// mtlsChangeCoalesce and mtlsChangeSplay delay the change mode after new
	// files were written: changes within the delay (a renewal and a new CA
	// at once) lead to one restart or signal, and the random splay spreads
	// the tasks of a node that learn about a new CA at the same moment.
	mtlsChangeCoalesce = time.Second
	mtlsChangeSplay    = 5 * time.Second

	// mtlsChangeMaxWait bounds how long the change mode waits for the
	// re-issued certificate after a CA rotation, and mtlsChangePoll how often
	// it checks.
	mtlsChangeMaxWait = 2 * time.Minute
	mtlsChangePoll    = 250 * time.Millisecond
)

// mtlsChange is a set of changed mTLS files.
type mtlsChange uint8

const (
	// mtlsChangeCert: a new workload certificate and key.
	mtlsChangeCert mtlsChange = 1 << iota

	// mtlsChangeBundle: a trust bundle with a CA the previous one didn't
	// have. Removing a retired CA is not a change the task must react to.
	mtlsChangeBundle
)

// String returns the task event message of the changes.
func (c mtlsChange) String() string {
	switch c {
	case mtlsChangeCert:
		return "mTLS certificate renewed"
	case mtlsChangeBundle:
		return "mTLS trust bundle rotated"
	default:
		return "mTLS certificate renewed and trust bundle rotated"
	}
}

// mtlsHook gets a short-lived workload certificate for tasks of task groups
// with an mtls service, writes it with its key and the trust bundle into the
// secrets directory, renews it after two thirds of its lifetime, and
// re-issues it when the CA of the region rotates. The key pair is generated
// here; only the CSR is sent to the servers. After writing a new certificate
// or a bundle with a new CA, it applies the change mode of the app mode mtls
// services of the task (noop, signal or restart) to the running task.
type mtlsHook struct {
	alloc     *structs.Allocation
	taskName  string
	taskUser  string
	rpc       config.RPCer
	region    string
	nodeKey   string
	events    eventEmitter
	lifecycle ti.TaskLifecycle
	logger    log.Logger

	lock       sync.Mutex
	dir        string
	cancel     context.CancelFunc
	cert       *mtlsIssued
	rootIdx    uint64
	activeRoot string

	// changeLock guards the change mode state: the actions for a new
	// certificate and a new CA (updated with the allocation), and the
	// changes not applied yet. changeCh wakes up applyChanges.
	changeLock     sync.Mutex
	onCert         structs.MTLSChangeAction
	onBundle       structs.MTLSChangeAction
	pendingChanges mtlsChange
	changeCh       chan struct{}
	changeCoalesce time.Duration
	changeSplay    time.Duration

	// reissuing is set while the certificate is re-issued after a CA
	// rotation: the change mode waits for it, so the task is restarted or
	// signaled once, with both new files.
	reissuing atomic.Bool
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
	alloc     *structs.Allocation
	task      *structs.Task
	rpc       config.RPCer
	region    string
	nodeKey   string
	events    eventEmitter
	lifecycle ti.TaskLifecycle
	logger    log.Logger
}

func newMTLSHook(c *mtlsHookConfig) *mtlsHook {
	h := &mtlsHook{
		alloc:     c.alloc,
		taskName:  c.task.Name,
		taskUser:  c.task.User,
		rpc:       c.rpc,
		region:    c.region,
		nodeKey:   c.nodeKey,
		events:    c.events,
		lifecycle: c.lifecycle,
		changeCh:  make(chan struct{}, 1),

		changeCoalesce: mtlsChangeCoalesce,
		changeSplay:    mtlsChangeSplay,
	}
	h.logger = c.logger.Named(h.Name())
	h.setChangeActions(c.alloc)
	return h
}

// setChangeActions reads the change modes of the task from the job of the
// allocation.
func (h *mtlsHook) setChangeActions(alloc *structs.Allocation) {
	var cert, bundle structs.MTLSChangeAction
	if alloc != nil && alloc.Job != nil {
		if tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup); tg != nil {
			cert, bundle = tg.MTLSChangeActions(tg.LookupTask(h.taskName))
		}
	}
	h.changeLock.Lock()
	defer h.changeLock.Unlock()
	h.onCert, h.onBundle = cert, bundle
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

	// A task restored after a client restart is still running with the
	// files it read when it started, so new files are a change for it.
	// Otherwise the task is about to start and reads the current files.
	restored := h.lifecycle != nil && h.lifecycle.IsRunning()
	var changes mtlsChange

	// A restarted task keeps a certificate that is still valid; the renewal
	// loop is already running.
	if h.cert == nil || !time.Now().Before(h.renewAt(h.cert)) {
		roots, err := h.fetchRoots(ctx, 0, false)
		if err != nil {
			return err
		}
		added, err := h.writeBundle(roots)
		if err != nil {
			return err
		}
		if added {
			changes |= mtlsChangeBundle
		}
		h.rootIdx = roots.Index
		h.activeRoot = roots.ActiveRootID

		// After a client restart, keep the certificate the task uses
		// while it is valid and signed by the active CA.
		if h.cert == nil {
			h.cert = h.loadCert(roots)
		}
		if h.cert == nil || !time.Now().Before(h.renewAt(h.cert)) || h.cert.rootID != roots.ActiveRootID {
			if err := h.issueWithRetry(ctx); err != nil {
				return err
			}
			changes |= mtlsChangeCert
		}
	}

	if restored {
		if changes != 0 {
			h.changed(changes)
		}
	} else {
		h.changeLock.Lock()
		h.pendingChanges = 0
		h.changeLock.Unlock()
	}

	if h.cancel == nil {
		loopCtx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.run(loopCtx)
		go h.applyChanges(loopCtx)
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

// Update picks up changed change modes of an in-place job update.
func (h *mtlsHook) Update(_ context.Context, req *interfaces.TaskUpdateRequest, _ *interfaces.TaskUpdateResponse) error {
	h.setChangeActions(req.Alloc)
	return nil
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
			// A renewal in the meantime may have used the new CA.
			h.lock.Lock()
			current := h.cert != nil && h.cert.rootID == h.activeRoot
			h.lock.Unlock()
			if current {
				h.reissuing.Store(false)
				continue
			}
		}

		h.lock.Lock()
		err := h.issueWithRetry(ctx)
		h.lock.Unlock()
		if err == nil {
			h.changed(mtlsChangeCert)
		}
		h.reissuing.Store(false)
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
		var stale, added bool
		if changed {
			var err error
			if added, err = h.writeBundle(roots); err != nil {
				h.logger.Error("failed to write the mTLS trust bundle", "error", err)
			}
			h.activeRoot = roots.ActiveRootID
			stale = h.cert != nil && roots.ActiveRootID != "" && roots.ActiveRootID != h.cert.rootID
			if stale {
				h.reissuing.Store(true)
			}
		}
		h.lock.Unlock()

		if added {
			h.changed(mtlsChangeBundle)
		}
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

// writeBundle writes the trust bundle file when its contents changed, and
// returns whether the new bundle has a CA the previous file didn't have. The
// caller holds the lock.
func (h *mtlsHook) writeBundle(roots *structs.MTLSRootsResponse) (bool, error) {
	var b strings.Builder
	for _, root := range roots.Roots {
		if root == nil || root.RootCert == "" {
			continue
		}
		b.WriteString(strings.TrimSpace(root.RootCert))
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return false, errors.New("the trust bundle is empty")
	}
	bundle := []byte(b.String())
	old, _ := os.ReadFile(filepath.Join(h.dir, mtlsCAFile))
	if bytes.Equal(old, bundle) {
		return false, nil
	}
	if err := h.writeFile(mtlsCAFile, bundle); err != nil {
		return false, err
	}
	return mtlsBundleAdded(old, bundle), nil
}

// mtlsBundleAdded returns whether the PEM bundle next has a certificate prev
// doesn't have.
func mtlsBundleAdded(prev, next []byte) bool {
	have := make(map[string]struct{})
	for rest := prev; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		have[string(block.Bytes)] = struct{}{}
	}
	for rest := next; ; {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			return false
		}
		if _, ok := have[string(block.Bytes)]; !ok {
			return true
		}
	}
}

// loadCert returns the certificate a task restored after a client restart
// uses: the files of the mtls directory, if the key matches the certificate
// and a CA of the trust bundle signed it. The caller holds the lock.
func (h *mtlsHook) loadCert(roots *structs.MTLSRootsResponse) *mtlsIssued {
	certPEM, err := os.ReadFile(filepath.Join(h.dir, mtlsCertFile))
	if err != nil {
		return nil
	}
	keyPEM, err := os.ReadFile(filepath.Join(h.dir, mtlsKeyFile))
	if err != nil {
		return nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || len(leaf.URIs) != 1 || !time.Now().Before(leaf.NotAfter) {
		return nil
	}
	for _, root := range roots.Roots {
		if root == nil {
			continue
		}
		block, _ := pem.Decode([]byte(root.RootCert))
		if block == nil {
			continue
		}
		ca, err := x509.ParseCertificate(block.Bytes)
		if err != nil || leaf.CheckSignatureFrom(ca) != nil {
			continue
		}
		cert := &mtlsIssued{
			rootID:    root.ID,
			spiffeID:  leaf.URIs[0].String(),
			notBefore: leaf.NotBefore,
			notAfter:  leaf.NotAfter,
		}
		h.logger.Debug("using the existing mTLS certificate", "spiffe_id", cert.spiffeID, "not_after", cert.notAfter)
		return cert
	}
	return nil
}

// changed records new files for the change mode.
func (h *mtlsHook) changed(c mtlsChange) {
	h.changeLock.Lock()
	h.pendingChanges |= c
	h.changeLock.Unlock()
	select {
	case h.changeCh <- struct{}{}:
	default:
	}
}

// applyChanges applies the change mode after new files were written, until
// the context ends. Changes during a short delay with a random splay are
// coalesced; after a CA rotation, it also waits for the re-issued
// certificate, so the new CA in the bundle and the new certificate lead to a
// single restart or signal.
func (h *mtlsHook) applyChanges(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.changeCh:
		}

		deadline := time.Now().Add(mtlsChangeMaxWait)
		delay := h.changeCoalesce
		if h.changeSplay > 0 {
			delay += time.Duration(mrand.Int63n(int64(h.changeSplay)))
		}
		timer := time.NewTimer(delay)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if h.reissuing.Load() && time.Now().Before(deadline) {
					timer.Reset(mtlsChangePoll)
					continue
				}
				break wait
			}
		}

		// Changes recorded until now are applied by this round.
		select {
		case <-h.changeCh:
		default:
		}
		h.changeLock.Lock()
		changes := h.pendingChanges
		h.pendingChanges = 0
		onCert, onBundle := h.onCert, h.onBundle
		h.changeLock.Unlock()
		h.apply(ctx, changes, onCert, onBundle)
	}
}

// apply restarts or signals the running task for changed files.
func (h *mtlsHook) apply(ctx context.Context, changes mtlsChange, onCert, onBundle structs.MTLSChangeAction) {
	var action structs.MTLSChangeAction
	if changes&mtlsChangeCert != 0 {
		action = action.Merge(onCert)
	}
	if changes&mtlsChangeBundle != 0 {
		action = action.Merge(onBundle)
	}
	if action.IsNoop() || h.lifecycle == nil {
		return
	}
	// A task that isn't running reads the current files when it starts.
	if !h.lifecycle.IsRunning() {
		return
	}

	msg := changes.String()
	if action.Restart {
		h.logger.Info("restarting the task", "reason", msg)
		err := h.lifecycle.Restart(ctx, structs.NewTaskEvent(structs.TaskRestartSignal).SetDisplayMessage(msg), false)
		if err != nil && !errors.Is(err, ErrTaskNotRunning) && ctx.Err() == nil {
			h.logger.Error("failed to restart the task", "reason", msg, "error", err)
		}
		return
	}
	for _, name := range action.Signals {
		sig, err := signals.Parse(name)
		if err != nil {
			h.events.EmitEvent(structs.NewTaskEvent(structs.TaskHookFailed).
				SetDisplayMessage(fmt.Sprintf("mtls: invalid change_signal %q", name)))
			continue
		}
		h.logger.Debug("signaling the task", "signal", name, "reason", msg)
		event := structs.NewTaskEvent(structs.TaskSignaling).SetTaskSignal(sig).SetDisplayMessage(msg)
		err = h.lifecycle.Signal(event, name)
		if err == nil || errors.Is(err, ErrTaskNotRunning) {
			continue
		}
		// Like templates: a task that can't be told about its new
		// certificate is killed, and its restart policy applies.
		_ = h.lifecycle.Kill(ctx, structs.NewTaskEvent(structs.TaskKilling).SetFailsTask().
			SetDisplayMessage(fmt.Sprintf("mtls: failed to send signal %s: %v", name, err)))
		return
	}
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
