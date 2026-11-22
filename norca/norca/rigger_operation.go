// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/source"
)

// Stacks on the server (design/rig-integration.md 4.6b). A plan and the
// status of a stack answer at once; an apply and a destroy are operations
// (rigger.norca.io/Operation) that the leader runs with the token of the
// caller. The token lives only in the memory of the leader, so a running
// operation can't go on after a change of leader: the new leader marks it
// Interrupted, and an apply of the stack again finds the releases already
// done unchanged. A cancel stops an operation between two releases. One
// operation of a stack runs at a time.

const (
	// riggerLeaderCheck is how often a running operation checks that this
	// server is still the leader.
	riggerLeaderCheck = time.Second

	// riggerLogDefaultLimit and riggerLogMaxLimit bound the records of one
	// read of a log; riggerLogMaxWait the wait for the first one.
	riggerLogDefaultLimit = 500
	riggerLogMaxLimit     = 1000
	riggerLogMaxWait      = 30 * time.Second

	// riggerLogWaitSlice is how long a wait for records goes before it
	// reads the phase of the operation again.
	riggerLogWaitSlice = time.Second
)

// RiggerStackRequest names a stack and what to do with it: a published
// stack (Source: <namespace>/<name> of the global registry, or a git
// address; Version: a constraint), or a local stack sent as files (Files:
// the archive of rigger Stack.Archive). The namespace of the request is the
// namespace of the stack.
type RiggerStackRequest struct {
	Source, Version string
	Files           []byte
	// Values are the values of the variables, as a values file (HCL).
	Values string
	// Prune uninstalls the releases the stack no longer has (an apply).
	Prune bool
	// Adopt takes over releases another manager applied (an apply).
	Adopt bool
	structs.WriteRequest
}

// RiggerStackPlanResponse is the plan of a stack.
type RiggerStackPlanResponse struct {
	Plan     *rigger.StackPlan
	Warnings []string
	structs.WriteMeta
}

// RiggerStackStatusResponse is the status of a stack: the releases it owns,
// their revision and drift.
type RiggerStackStatusResponse struct {
	Stack    string
	Releases []rigger.ReleaseStatus
	structs.WriteMeta
}

// RiggerOperationNameRequest names an operation in the namespace of the
// request.
type RiggerOperationNameRequest struct {
	Name string
	structs.WriteRequest
}

// RiggerOperationResponse is an operation.
type RiggerOperationResponse struct {
	Operation *RiggerOperation
	structs.WriteMeta
}

// RiggerOperationLogRequest reads the log of an operation: the records after
// the sequence After, at most Limit, waiting up to Wait for the first one.
type RiggerOperationLogRequest struct {
	Name  string
	After uint64
	Limit int
	Wait  time.Duration
	structs.QueryOptions
}

// RiggerOperationLogResponse is a page of the log of an operation; Phase is
// the phase of the operation, Finished whether it has ended (no records
// will follow).
type RiggerOperationLogResponse struct {
	Lines    []RiggerLogLine
	Phase    string
	Finished bool
	structs.QueryMeta
}

// riggerOperations are the operations this server runs as the leader.
type riggerOperations struct {
	mu      sync.Mutex
	running map[string]*riggerRunning // by <namespace>/<stack>
}

// riggerRunning is a running operation.
type riggerRunning struct {
	name      string
	cancel    context.CancelFunc
	cancelled bool
}

func riggerStackKey(namespace, stack string) string { return namespace + "/" + stack }

// reserve takes the place of the one running operation of a stack, or
// refuses it (409) while another runs.
func (o *riggerOperations) reserve(key, stack string) (*riggerRunning, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.running == nil {
		o.running = map[string]*riggerRunning{}
	}

	if running := o.running[key]; running != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusConflict,
			"the stack %s has a running operation, %s: cancel it or wait for its end", stack, running.name)
	}

	run := &riggerRunning{name: "pending"}
	o.running[key] = run

	return run, nil
}

// release frees the place of the running operation of a stack.
func (o *riggerOperations) release(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	delete(o.running, key)
}

// isRunning reports whether this server runs the operation.
func (o *riggerOperations) isRunning(namespace, stack, name string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	r := o.running[riggerStackKey(namespace, stack)]

	return r != nil && r.name == name
}

// StackPlan plans a stack: what an apply would do to each release. It runs
// on the leader with the caller's token.
func (r *Rigger) StackPlan(args *RiggerStackRequest, reply *RiggerStackPlanResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.StackPlan", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.StackPlan", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	eng, s, values, err := r.srv.riggerStack(ctx, args, r.ctx)
	if err != nil {
		return err
	}

	plan, diags := eng.PlanStack(ctx, s, values)
	if diags.HasErrors() {
		return riggerDiagsError(diags)
	}

	reply.Plan, reply.Warnings = plan, diagWarnings(diags)
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// StackStatus answers with the releases a stack owns, their revision and
// their drift.
func (r *Rigger) StackStatus(args *RiggerStackRequest, reply *RiggerStackStatusResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.StackStatus", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.StackStatus", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	eng, s, _, err := r.srv.riggerStack(ctx, args, r.ctx)
	if err != nil {
		return err
	}

	releases, err := eng.StackStatus(ctx, s)
	if err != nil {
		return err
	}

	reply.Stack, reply.Releases = s.Name(), releases
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// StackApply plans a stack and runs its apply as an operation on the
// leader; it answers with the operation at once.
func (r *Rigger) StackApply(args *RiggerStackRequest, reply *RiggerOperationResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.StackApply", &retErr)

	return r.stackOperation(args, reply, riggerOperationApply, "Rigger.StackApply")
}

// StackDestroy runs the uninstall of the releases of a stack as an
// operation on the leader; it answers with the operation at once.
func (r *Rigger) StackDestroy(args *RiggerStackRequest, reply *RiggerOperationResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.StackDestroy", &retErr)

	return r.stackOperation(args, reply, riggerOperationDestroy, "Rigger.StackDestroy")
}

func (r *Rigger) stackOperation(args *RiggerStackRequest, reply *RiggerOperationResponse, action, method string) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward(method, args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	op, err := r.srv.startRiggerOperation(args, action, r.ctx)
	if err != nil {
		return err
	}

	reply.Operation = op
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// CancelOperation asks a running operation to stop before its next
// release. It needs the right to write releases in the namespace.
func (r *Rigger) CancelOperation(args *RiggerOperationNameRequest, reply *RiggerOperationResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.CancelOperation", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.CancelOperation", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	namespace := args.RequestNamespace()
	if err := r.srv.authorizeNamespaceObject(args, namespace, structs.ReleaseKindRef, acl.ObjectCapabilityWrite); err != nil {
		return err
	}

	op, err := r.srv.cancelRiggerOperation(namespace, args.Name)
	if err != nil {
		return err
	}

	reply.Operation = op
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// OperationLog reads a page of the log of an operation from the bus. It
// needs read on operations in the namespace.
func (r *Rigger) OperationLog(args *RiggerOperationLogRequest, reply *RiggerOperationLogResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.OperationLog", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.OperationLog", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	namespace := args.RequestNamespace()
	if err := r.srv.authorizeNamespaceObject(args, namespace, structs.RiggerOperationKindRef, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	op, _, err := r.srv.riggerOperation(namespace, args.Name)
	if err != nil {
		return err
	}

	limit := cmp.Or(args.Limit, riggerLogDefaultLimit)
	limit = min(max(limit, 1), riggerLogMaxLimit)
	wait := min(max(args.Wait, 0), riggerLogMaxWait)

	ctx, cancel := context.WithTimeout(context.Background(), wait+10*time.Second)
	defer cancel()

	// The wait goes in slices, the phase read between them: the end of an
	// operation writes no record, and a reader that has read its last one
	// learns of the end within a slice, not after the whole wait.
	var lines []RiggerLogLine

	deadline := time.Now().Add(wait)

	for {
		slice := min(time.Until(deadline), riggerLogWaitSlice)
		if isFinishedRiggerOperation(op.Status.Phase) || slice < 0 {
			slice = 0
		}

		finished := isFinishedRiggerOperation(op.Status.Phase)

		if lines, err = r.srv.readRiggerLog(ctx, op.Status.LogSubject, args.After, limit, slice); err != nil {
			return err
		}

		if len(lines) > 0 || finished || slice == 0 {
			break
		}

		if now, _, err := r.srv.riggerOperation(namespace, args.Name); err == nil {
			op = now
		}
	}

	// The phase after the read: a log read to its end of a finished
	// operation has nothing more; a full page may have more after it.
	if now, _, err := r.srv.riggerOperation(namespace, args.Name); err == nil {
		op = now
	}

	reply.Lines, reply.Phase = lines, op.Status.Phase
	reply.Finished = isFinishedRiggerOperation(op.Status.Phase) && len(lines) < limit
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// authorizeNamespaceObject checks an object capability on a kind in a
// namespace.
func (s *Server) authorizeNamespaceObject(args structs.RequestWithIdentity, namespace, kindRef, capability string) error {
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return err
	}

	if aclObj != nil && !aclObj.AllowObjectOperation(namespace, kindRef, capability) {
		return structs.ErrPermissionDenied
	}

	return nil
}

// riggerStack checks the rights of a stack request and loads the stack and
// its values with an engine of the caller's token: a published stack from
// its source (the global registry at the commit of its last sync), a local
// one from its files. The stack is in the namespace of the request.
func (s *Server) riggerStack(ctx context.Context, args *RiggerStackRequest, rpcCtx *RPCContext, opts ...rigger.Option) (*rigger.Engine, *rigger.Stack, rigger.Values, error) {
	namespace := args.RequestNamespace()
	local := len(args.Files) > 0

	if local == (args.Source != "") {
		return nil, nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "a stack is either published (a source) or sent as files")
	}

	if err := s.authorizeNamespaceObject(args, namespace, structs.ReleaseKindRef, acl.ObjectCapabilityWrite); err != nil {
		return nil, nil, nil, err
	}

	if !local {
		if err := s.authorizeNamespaceObject(args, acl.ObjectClusterNamespace, structs.RiggerRegistryKindRef, acl.ObjectCapabilityRead); err != nil {
			return nil, nil, nil, err
		}
	}

	fetcher, err := s.riggerRequestFetcher()
	if err != nil {
		return nil, nil, nil, err
	}

	if err := s.prepareRiggerGlobal(ctx, fetcher, args.Source); err != nil {
		return nil, nil, nil, err
	}

	eng, err := s.riggerServerEngine(args.AuthToken, namespace, !local, rpcCtx, fetcher, opts...)
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		st    *rigger.Stack
		diags hcl.Diagnostics
	)

	if local {
		st, diags = eng.LoadStackFiles(ctx, args.Files)
	} else {
		st, diags = eng.FetchStack(ctx, args.Source, args.Version)
	}

	if diags.HasErrors() {
		return nil, nil, nil, riggerDiagsError(diags)
	}

	if st.Namespace() != namespace {
		return nil, nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
			"the stack %s is of the namespace %q: send the request in that namespace", st.Name(), st.Namespace())
	}

	var values rigger.Values
	if args.Values != "" {
		if values, diags = rigger.ParseValues("values.vars.hcl", []byte(args.Values)); diags.HasErrors() {
			return nil, nil, nil, riggerDiagsError(diags)
		}
	}

	return eng, st, values, nil
}

// prepareRiggerGlobal points the fetcher of a request at the index of the
// global registry at the commit of its last sync, or leaves it the empty
// index when it has none: a stack never reaches the global registry past a
// sync. A published stack of the global registry needs the sync.
func (s *Server) prepareRiggerGlobal(ctx context.Context, fetcher *fetch.Fetcher, address string) error {
	r, err := s.riggerRegistriesOf()
	if err != nil {
		return err
	}

	obj, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", structs.RiggerGlobalRegistry)
	if err != nil {
		return err
	}

	if obj != nil {
		reg, err := riggerRegistryFromObject(obj)
		if err != nil {
			return err
		}

		if reg.Status.Commit != "" {
			return r.useGlobal(ctx, fetcher, reg)
		}
	}

	if address != "" {
		if req, err := source.Parse(address, nil); err == nil && req.Kind == source.KindGlobal {
			return errRiggerRegistryNotSynced(structs.RiggerGlobalRegistry)
		}
	}

	return nil
}

// noGlobalDir is an empty index: that of a global registry never synced.
func (r *riggerRegistries) noGlobalDir() (string, error) {
	dir := filepath.Join(filepath.Dir(r.fetcher.TreeDir("x")), "..", "global-not-synced")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}

	return filepath.Abs(dir)
}

// startRiggerOperation plans an apply or a destroy of a stack, records it as
// an operation and runs it on this server, the leader.
func (s *Server) startRiggerOperation(args *RiggerStackRequest, action string, rpcCtx *RPCContext) (*RiggerOperation, error) {
	opID := uuid.Generate()
	subject := s.riggerLogSubject(opID)
	w := s.newRiggerLogWriter(subject)

	// Until the goroutine of the operation takes them over, the log and the
	// place of the stack are let go on every way out, a panic too.
	started := false

	defer func() {
		if !started {
			w.close()
		}
	}()

	logger := slog.New(teeHandler{a: &hclogHandler{logger: s.logger.Named("rigger")}, b: &busLogHandler{w: w}})

	planCtx, cancelPlan := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancelPlan()

	eng, st, values, err := s.riggerStack(planCtx, args, rpcCtx, rigger.WithLogger(logger))
	if err != nil {
		return nil, err
	}

	namespace := st.Namespace()
	key := riggerStackKey(namespace, st.Name())

	// Reserved before the plan, so that two calls do not both start.
	run, err := s.riggerOps.reserve(key, st.Name())
	if err != nil {
		return nil, err
	}

	release := func() { s.riggerOps.release(key) }

	defer func() {
		if !started {
			release()
		}
	}()

	var (
		plan  *rigger.StackPlan
		steps []RiggerOperationRelease
	)

	switch action {
	case riggerOperationApply:
		var diags hcl.Diagnostics

		if plan, diags = eng.PlanStack(planCtx, st, values); diags.HasErrors() {
			return nil, riggerDiagsError(diags)
		}

		for _, rp := range plan.Releases {
			steps = append(steps, RiggerOperationRelease{Namespace: rp.Release.Namespace, Release: rp.Release.Name, Action: string(rp.Action)})
		}
	default:
		owned, err := eng.StackStatus(planCtx, st)
		if err != nil {
			return nil, err
		}

		for _, rs := range owned {
			steps = append(steps, RiggerOperationRelease{Namespace: rs.Revision.Release.Namespace, Release: rs.Revision.Release.Name, Action: "remove"})
		}
	}

	op := &RiggerOperation{
		Namespace: namespace,
		Name:      riggerOperationName(st.Name()),
		Spec: RiggerOperationSpec{
			Stack: st.Name(), Action: action, Source: args.Source, Version: args.Version,
			Prune: args.Prune, Adopt: args.Adopt, RequestedBy: s.tokenAccessor(args),
		},
		Status: RiggerOperationStatus{
			Phase: riggerOpRunning, Plan: steps, LogSubject: subject,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}

	if len(args.Files) > 0 {
		sum := sha256.Sum256(args.Files)
		op.Spec.FilesDigest = "sha256:" + hex.EncodeToString(sum[:])
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Named before the operation is written: the operator of the leader
	// takes a Running operation it does not run as Interrupted.
	s.riggerOps.mu.Lock()
	run.name, run.cancel = op.Name, cancel
	s.riggerOps.mu.Unlock()

	obj, err := s.writeRiggerOperation(nil, op)
	if err != nil {
		cancel()

		return nil, err
	}

	started = true

	go s.runRiggerOperation(ctx, cancel, run, release, w, eng, st, plan, op, obj)

	return op, nil
}

// riggerOperationName is the name of a new operation of a stack: the stack,
// the time and a few random characters.
func riggerOperationName(stack string) string {
	name := fmt.Sprintf("%s-%s-%s", stack, time.Now().UTC().Format("20060102-150405"), uuid.Generate()[:4])

	return strings.ToLower(name)
}

// tokenAccessor is the accessor of the token of a request, for the record
// of who asked; empty without ACLs.
func (s *Server) tokenAccessor(args structs.RequestWithIdentity) string {
	if id := args.GetIdentity(); id != nil && id.ACLToken != nil {
		return id.ACLToken.AccessorID
	}

	return ""
}

// runRiggerOperation runs an operation until it ends, is cancelled, or this
// server is no longer the leader, and writes its end.
func (s *Server) runRiggerOperation(ctx context.Context, cancel context.CancelFunc, run *riggerRunning, release func(),
	w *riggerLogWriter, eng *rigger.Engine, st *rigger.Stack, plan *rigger.StackPlan, op *RiggerOperation, obj *structs.CustomObject) {
	defer release()
	defer cancel()

	// Nobody waits for the error of this goroutine: after a panic it ends
	// the log and writes the end of the operation itself, before its place
	// is let go.
	var panicked error

	logClosed := false

	defer func() {
		if panicked == nil {
			return
		}

		if !logClosed {
			w.close()
		}

		s.finishRiggerOperation(op.Namespace, op.Name, func(status *RiggerOperationStatus) {
			if status.Phase == riggerOpRunning {
				status.Phase = riggerOpFailed
				status.FinishedAt = time.Now().UTC().Format(time.RFC3339)
				status.Error = rigger.ErrInternal.Error()
			}
		})
	}()
	defer s.recoverRigger("operation "+op.Name, &panicked)

	var deposed sync.Once

	lost := make(chan struct{})

	go func() {
		t := time.NewTicker(riggerLeaderCheck)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !s.IsLeader() {
					deposed.Do(func() { close(lost) })
					cancel()

					return
				}
			}
		}
	}()

	var (
		outcomes []RiggerOperationRelease
		runErr   error
	)

	if op.Spec.Action == riggerOperationApply {
		out, diags := eng.ApplyStack(ctx, plan, rigger.ApplyOptions{Prune: op.Spec.Prune, Adopt: op.Spec.Adopt})
		for _, o := range out {
			outcomes = append(outcomes, RiggerOperationRelease{
				Namespace: o.Release.Namespace, Release: o.Release.Name, Action: string(o.Action), Revision: o.Revision, Skipped: o.Skipped,
			})
		}

		if diags.HasErrors() {
			runErr = errors.New(diags.Error())
		}
	} else {
		removed, err := eng.DestroyStack(ctx, st)
		for _, ref := range removed {
			outcomes = append(outcomes, RiggerOperationRelease{Namespace: ref.Namespace, Release: ref.Name, Action: "remove"})
		}

		runErr = err
	}

	logClosed = true
	dropped := w.close()

	select {
	case <-lost:
		// The new leader marks the operation Interrupted.
		return
	default:
	}

	s.riggerOps.mu.Lock()
	cancelled := run.cancelled
	s.riggerOps.mu.Unlock()

	phase := riggerOpSucceeded

	switch {
	case runErr != nil && cancelled:
		phase = riggerOpCancelled
	case runErr != nil:
		phase = riggerOpFailed
	}

	s.finishRiggerOperation(op.Namespace, op.Name, func(st *RiggerOperationStatus) {
		st.Phase, st.Outcomes = phase, outcomes
		st.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		st.LogStored, st.LogDropped = w.stored.Load(), dropped

		if runErr != nil {
			st.Error = operatorMessage(runErr.Error())
		}
	})

	if o, err := s.State().CustomObjectByName(nil, obj.Group, obj.Kind, obj.Namespace, obj.Name); err == nil && o != nil {
		s.operatorEvent(o, eventTypeOf(phase), phase, fmt.Sprintf("%s of the stack %s: %s", op.Spec.Action, op.Spec.Stack, phase))
	}
}

func eventTypeOf(phase string) string {
	if phase == riggerOpSucceeded {
		return "Normal"
	}

	return "Warning"
}

// finishRiggerOperation changes the status of an operation as it is stored,
// with a check-and-set, trying again after a concurrent write (a cancel).
func (s *Server) finishRiggerOperation(namespace, name string, change func(*RiggerOperationStatus)) {
	for range 5 {
		op, obj, err := s.riggerOperation(namespace, name)
		if err != nil {
			s.logger.Named("rigger").Warn("the end of an operation could not be written", "operation", namespace+"/"+name, "error", err)

			return
		}

		change(&op.Status)

		if _, err := s.writeRiggerOperation(obj, op); err == nil {
			return
		} else if code, _, ok := structs.CodeFromRPCCodedErr(err); (!ok || code != http.StatusConflict) && !strings.Contains(err.Error(), "modify index") {
			s.logger.Named("rigger").Warn("the end of an operation could not be written", "operation", namespace+"/"+name, "error", err)

			return
		}
	}
}

// writeRiggerOperation writes an operation: a new one (stored nil) or a
// change of the stored one, with a check-and-set on its modify index.
func (s *Server) writeRiggerOperation(stored *structs.CustomObject, op *RiggerOperation) (*structs.CustomObject, error) {
	spec, err := objectMap(op.Spec)
	if err != nil {
		return nil, err
	}

	status, err := objectMap(op.Status)
	if err != nil {
		return nil, err
	}

	obj := &structs.CustomObject{
		Group: structs.RiggerObjectGroup, Kind: structs.RiggerOperationKind, Namespace: op.Namespace, Name: op.Name,
		ManagedBy: structs.OperatorsManagedBy,
	}
	if stored != nil {
		obj = stored.Copy()
	}

	obj.Spec, obj.Status = spec, status

	return s.writeOperatorObject(obj)
}

// riggerOperation reads an operation.
func (s *Server) riggerOperation(namespace, name string) (*RiggerOperation, *structs.CustomObject, error) {
	obj, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerOperationKind, namespace, name)
	if err != nil {
		return nil, nil, err
	}

	if obj == nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %s/%s not found", structs.RiggerOperationKindRef, namespace, name)
	}

	op, err := riggerOperationFromObject(obj)

	return op, obj, err
}

// cancelRiggerOperation asks a running operation of this server to stop
// before its next release, and records the cancel.
func (s *Server) cancelRiggerOperation(namespace, name string) (*RiggerOperation, error) {
	op, _, err := s.riggerOperation(namespace, name)
	if err != nil {
		return nil, err
	}

	if op.Status.Phase != riggerOpRunning {
		return nil, structs.NewErrRPCCodedf(http.StatusConflict, "the operation %s has ended: %s", name, op.Status.Phase)
	}

	s.riggerOps.mu.Lock()
	run := s.riggerOps.running[riggerStackKey(namespace, op.Spec.Stack)]

	if run != nil && run.name == name && run.cancel != nil {
		run.cancelled = true
		run.cancel()
	}
	s.riggerOps.mu.Unlock()

	s.finishRiggerOperation(namespace, name, func(st *RiggerOperationStatus) {
		if st.Phase == riggerOpRunning {
			st.CancelRequested = true
		}
	})

	op, _, err = s.riggerOperation(namespace, name)

	return op, err
}

// riggerOperationOperator marks the running operations the leader does not
// run as Interrupted (their leader changed), and keeps the last
// riggerOperationsKept operations of each stack, purging the logs of those
// it deletes.
type riggerOperationOperator struct {
	srv    *Server
	logger hclog.Logger
}

func newRiggerOperationOperator(srv *Server, logger hclog.Logger) *riggerOperationOperator {
	return &riggerOperationOperator{srv: srv, logger: logger.Named("rigger_operation")}
}

func (o *riggerOperationOperator) kinds() []*structs.ObjectKind { return builtinRiggerOperationKinds() }

func (o *riggerOperationOperator) reconcile(ws memdb.WatchSet) (time.Time, error) {
	iter, err := o.srv.State().CustomObjects(ws, structs.RiggerObjectGroup, structs.RiggerOperationKind, structs.AllNamespacesSentinel, state.SortDefault)
	if err != nil {
		return time.Time{}, err
	}

	byStack := map[string][]*structs.CustomObject{}

	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)

		op, err := riggerOperationFromObject(obj)
		if err != nil {
			continue
		}

		if op.Status.Phase == riggerOpRunning && !o.srv.riggerOps.isRunning(op.Namespace, op.Spec.Stack, op.Name) {
			o.srv.finishRiggerOperation(op.Namespace, op.Name, func(st *RiggerOperationStatus) {
				if st.Phase == riggerOpRunning {
					st.Phase = riggerOpInterrupted
					st.FinishedAt = time.Now().UTC().Format(time.RFC3339)
					st.Error = "the server that ran it is no longer the leader: apply the stack again (the releases already done come back unchanged)"
				}
			})

			continue
		}

		key := riggerStackKey(op.Namespace, op.Spec.Stack)
		byStack[key] = append(byStack[key], obj)
	}

	for _, objs := range byStack {
		o.keepLast(objs)
	}

	return time.Time{}, nil
}

// keepLast deletes the finished operations of a stack beyond the newest
// riggerOperationsKept, and purges their logs.
func (o *riggerOperationOperator) keepLast(objs []*structs.CustomObject) {
	if len(objs) <= riggerOperationsKept {
		return
	}

	slices.SortFunc(objs, func(a, b *structs.CustomObject) int { return cmp.Compare(b.CreateIndex, a.CreateIndex) })

	for _, obj := range objs[riggerOperationsKept:] {
		op, err := riggerOperationFromObject(obj)
		if err != nil || !isFinishedRiggerOperation(op.Status.Phase) {
			continue
		}

		if _, err := o.srv.removeCustomObject(obj, ""); err != nil {
			o.logger.Debug("an old operation could not be deleted", "operation", obj.Namespace+"/"+obj.Name, "error", err)

			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := o.srv.purgeRiggerLog(ctx, op.Status.LogSubject); err != nil {
			o.logger.Debug("the log of an old operation could not be purged", "operation", obj.Namespace+"/"+obj.Name, "error", err)
		}
		cancel()
	}
}

// diagWarnings are the warnings of diagnostics.
func diagWarnings(diags hcl.Diagnostics) []string {
	var out []string

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			out = append(out, strings.TrimSpace(d.Summary+": "+d.Detail))
		}
	}

	return out
}
