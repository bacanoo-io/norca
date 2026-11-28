// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/evalwire"
	"github.com/bacanoo-io/norca/rigger/oplog"
)

// EvaluationProcess is a child process an engine evaluates in
// (rig-integration.md 4.6d): it reads requests from what is written to it
// and answers on what is read from it. Close ends it; Wait then says how it
// ended (killed, out of memory, a stack overflow).
type EvaluationProcess interface {
	io.Reader
	io.Writer
	Close() error
	Wait() error
}

// ProcessStarter starts a child process that serves evaluations
// (ServeEvaluations).
type ProcessStarter func(ctx context.Context) (EvaluationProcess, error)

// WithChildEvaluation has the engine evaluate the stacks and modules it
// plans (PlanStack, PlanInstall) in a child process that start starts: the
// first plan starts it, the plans after it use it, and Close ends it. A
// stack overflow or an allocation past the memory of the child ends the
// child, and the plan fails; the host goes on. The other evaluations of the
// engine (Evaluate, the validations, the tests) stay in its process.
func WithChildEvaluation(start ProcessStarter) Option {
	return func(e *Engine) error {
		if start == nil {
			return invalid("process starter")
		}

		e.child = &childEvaluator{start: start}

		return nil
	}
}

// Close ends the child process of the evaluations, when there is one.
func (e *Engine) Close() error {
	if e.child == nil {
		return nil
	}

	return e.child.close()
}

// transferTime is what the child gets on top of the evaluation limit, for
// the files and the answer; tests shorten it.
var transferTime = 15 * time.Second

// ErrEvaluationProcess is a child process of the evaluations that ended
// before it answered.
var ErrEvaluationProcess = errors.New("the evaluation process ended")

// childEvaluator sends evaluations to a child process, one at a time.
type childEvaluator struct {
	start ProcessStarter

	mu   sync.Mutex
	proc EvaluationProcess
}

// call sends one request and reads its answer. A child that does not
// answer within the limit is ended; one that ended is not used again.
func (c *childEvaluator) call(ctx context.Context, limit time.Duration, req *evalwire.Request) (*evalwire.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.proc == nil {
		proc, err := c.start(ctx)
		if err != nil {
			return nil, fmt.Errorf("starting the evaluation process: %w", err)
		}

		c.proc = proc
	}

	proc := c.proc

	ctx, cancel := context.WithTimeout(ctx, limit+transferTime)
	defer cancel()

	// The child is ended when the context ends: the read below returns.
	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-ctx.Done():
			_ = proc.Close()
		case <-done:
		}
	}()

	var resp evalwire.Response

	err := evalwire.WriteFrame(proc, req)
	if err == nil {
		err = evalwire.ReadFrame(proc, &resp)
	}

	if err != nil {
		c.proc = nil
		_ = proc.Close()

		why := proc.Wait()
		if ctx.Err() != nil {
			why = fmt.Errorf("no answer within %s: %w", limit+transferTime, ctx.Err())
		}

		if why == nil {
			why = err
		}

		return nil, fmt.Errorf("%w: %w", ErrEvaluationProcess, why)
	}

	return &resp, nil
}

// close ends the child.
func (c *childEvaluator) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.proc == nil {
		return nil
	}

	proc := c.proc
	c.proc = nil

	// Closing the requests ends a child that waits for one.
	if err := proc.Close(); err != nil {
		return err
	}

	_ = proc.Wait()

	return nil
}

// wireLimits are the limits of the engine on the wire.
func (e *Engine) wireLimits() evalwire.Limits {
	return evalwire.Limits{
		ModuleBytes: e.limits.ModuleBytes, FileBytes: e.limits.FileBytes, Resources: e.limits.Resources,
		Depth: e.limits.Depth, Modules: e.limits.Modules, Duration: e.limits.Duration,
	}
}

// evaluateStack gives the variables of s their values and evaluates the
// stack and its releases, in this process or in the child. existing are
// the generated values the stack has; it returns those it generated now.
func (e *Engine) evaluateStack(ctx context.Context, s *Stack, values Values, existing map[string]string) (*eval.StackResult, map[string]string, hcl.Diagnostics) {
	if e.child == nil {
		return e.evaluateStackHere(ctx, s, values, existing)
	}

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "stack.evaluate", "stack", s.Name(), "process", "child")

	res, generated, diags := e.evaluateStackInChild(ctx, s, values, existing)
	span.End(ctx, diagsErr(diags))

	return res, generated, diags
}

// evaluateStackHere is evaluateStack in this process.
func (e *Engine) evaluateStackHere(ctx context.Context, s *Stack, values Values, existing map[string]string) (*eval.StackResult, map[string]string, hcl.Diagnostics) {
	vars, diags := eval.Variables(s.s.Vars, inputs(values))
	if diags.HasErrors() {
		return nil, nil, diags
	}

	evalCtx, cancel := e.evaluationContext(ctx)
	defer cancel()

	generated := map[string]string{}

	res, moreDiags := eval.EvaluateStack(evalCtx, s.s, vars, generatedValues(existing, generated), e.evalConfig())

	return res, generated, append(diags, moreDiags...)
}

// generatedValues answers the generated values of an evaluation: an
// existing one, else one generated now and kept in generated.
func generatedValues(existing, generated map[string]string) eval.Generated {
	return func(name string) (string, bool) {
		if v, ok := existing[name]; ok {
			return v, true
		}

		if _, ok := generated[name]; !ok {
			generated[name] = generate()
		}

		return generated[name], false
	}
}

// evaluateStackInChild is evaluateStack in the child.
func (e *Engine) evaluateStackInChild(ctx context.Context, s *Stack, values Values, existing map[string]string) (*eval.StackResult, map[string]string, hcl.Diagnostics) {
	tree, err := evalwire.CollectStack(s.s)
	if err != nil {
		return nil, nil, errorDiags("Stack not sent to the evaluation", err)
	}

	in, err := evalwire.EncodeInputs(inputs(values))
	if err != nil {
		return nil, nil, errorDiags("Values not sent to the evaluation", err)
	}

	resp, err := e.child.call(ctx, e.limits.Duration, &evalwire.Request{
		Limits: e.wireLimits(),
		Stack:  &evalwire.StackRequest{Tree: tree, Values: in, Existing: existing},
	})
	if err != nil {
		return nil, nil, errorDiags("Evaluation failed", err)
	}

	diags := evalwire.DecodeDiagnostics(resp.Diagnostics)
	if diags.HasErrors() || resp.Stack == nil {
		return nil, nil, diags
	}

	res, err := resp.Stack.Decode(s.s)
	if err != nil {
		return nil, nil, append(diags, errorDiags("Evaluation not read", err)...)
	}

	generated := resp.Stack.Generated
	if generated == nil {
		generated = map[string]string{}
	}

	return res, generated, diags
}

// evaluateModule gives the variables of m their values and evaluates it for
// an install, in this process or in the child; it returns the object of the
// variables too.
func (e *Engine) evaluateModule(ctx context.Context, m *Module, values Values, facts eval.Facts) (cty.Value, *eval.Result, hcl.Diagnostics) {
	if e.child == nil {
		vars, diags := e.variables(ctx, m, values)
		if diags.HasErrors() {
			return cty.NilVal, nil, diags
		}

		evalCtx, cancel := e.evaluationContext(ctx)
		defer cancel()

		res, moreDiags := eval.Evaluate(evalCtx, m.mod, vars, facts, e.evalConfig())

		return vars, res, append(diags, moreDiags...)
	}

	ctx, span := oplog.Start(ctx, e.logger, slog.LevelDebug, "module.evaluate", "module", m.mod.Metadata.Name, "process", "child")

	vars, res, diags := e.evaluateModuleInChild(ctx, m, values, facts)
	span.End(ctx, diagsErr(diags))

	return vars, res, diags
}

// evaluateModuleInChild is evaluateModule in the child.
func (e *Engine) evaluateModuleInChild(ctx context.Context, m *Module, values Values, facts eval.Facts) (cty.Value, *eval.Result, hcl.Diagnostics) {
	tree, err := evalwire.CollectModule(m.mod)
	if err != nil {
		return cty.NilVal, nil, errorDiags("Module not sent to the evaluation", err)
	}

	in, err := evalwire.EncodeInputs(inputs(values))
	if err != nil {
		return cty.NilVal, nil, errorDiags("Values not sent to the evaluation", err)
	}

	resp, err := e.child.call(ctx, e.limits.Duration, &evalwire.Request{
		Limits: e.wireLimits(),
		Module: &evalwire.ModuleRequest{Tree: tree, Values: in, Facts: evalwire.EncodeFacts(facts)},
	})
	if err != nil {
		return cty.NilVal, nil, errorDiags("Evaluation failed", err)
	}

	diags := evalwire.DecodeDiagnostics(resp.Diagnostics)
	if diags.HasErrors() || resp.Module == nil {
		return cty.NilVal, nil, diags
	}

	vars, err := resp.Module.Vars.Decode()
	if err != nil {
		return cty.NilVal, nil, append(diags, errorDiags("Evaluation not read", err)...)
	}

	res, err := resp.Module.Result.Decode()
	if err != nil {
		return cty.NilVal, nil, append(diags, errorDiags("Evaluation not read", err)...)
	}

	return vars, res, diags
}

// ServeEvaluations is the child side of WithChildEvaluation: it answers
// the requests read from r on w, one at a time, until r ends, with an
// engine of opts (its job and resource decoders and kinds; the limits come
// with each request). A panic of an evaluation becomes the error of its
// answer, as in an engine; a stack overflow or an allocation past the
// memory ends the process, which is the point.
func ServeEvaluations(ctx context.Context, r io.Reader, w io.Writer, opts ...Option) error {
	e, err := New(opts...)
	if err != nil {
		return err
	}

	for {
		var req evalwire.Request
		if err := evalwire.ReadFrame(r, &req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("reading a request: %w", err)
		}

		resp := e.serve(ctx, &req)

		if err := evalwire.WriteFrame(w, resp); err != nil {
			return fmt.Errorf("writing an answer: %w", err)
		}
	}
}

// serve answers one request.
func (e *Engine) serve(ctx context.Context, req *evalwire.Request) (resp *evalwire.Response) {
	resp = &evalwire.Response{}

	var diags hcl.Diagnostics

	defer func() {
		e.recoverDiags(ctx, "child.evaluate", &diags)
		resp.Diagnostics = evalwire.EncodeDiagnostics(diags)
	}()

	l := req.Limits

	limits := Limits{
		ModuleBytes: l.ModuleBytes, FileBytes: l.FileBytes, Resources: l.Resources,
		Depth: l.Depth, Modules: l.Modules, Duration: l.Duration,
	}
	if err := limits.validate(); err != nil {
		diags = errorDiags("Invalid limits", err)

		return resp
	}

	e.limits = limits

	switch {
	case req.Stack != nil:
		resp.Stack, diags = e.serveStack(ctx, req.Stack)
	case req.Module != nil:
		resp.Module, diags = e.serveModule(ctx, req.Module)
	default:
		diags = errorDiags("Empty request", errors.New("the request names no stack and no module"))
	}

	return resp
}

// serveStack loads and evaluates a stack in the child.
func (e *Engine) serveStack(ctx context.Context, req *evalwire.StackRequest) (*evalwire.StackResponse, hcl.Diagnostics) {
	s, diags := req.Tree.Load(ctx, e.langLimits())
	if diags.HasErrors() {
		return nil, diags
	}

	in, err := evalwire.DecodeInputs(req.Values)
	if err != nil {
		return nil, append(diags, errorDiags("Values not read", err)...)
	}

	values := make(Values, len(in))
	for name, v := range in {
		values[name] = Value{Value: v.Value, Range: v.Range}
	}

	res, generated, moreDiags := e.evaluateStackHere(ctx, &Stack{s: s}, values, req.Existing)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	out, err := evalwire.EncodeStackResult(res, generated)
	if err != nil {
		return nil, append(diags, errorDiags("Evaluation not sent", err)...)
	}

	return out, diags
}

// serveModule loads and evaluates a module in the child.
func (e *Engine) serveModule(ctx context.Context, req *evalwire.ModuleRequest) (*evalwire.ModuleResponse, hcl.Diagnostics) {
	mod, diags := req.Tree.Load(ctx, e.langLimits())
	if diags.HasErrors() {
		return nil, diags
	}

	in, err := evalwire.DecodeInputs(req.Values)
	if err != nil {
		return nil, append(diags, errorDiags("Values not read", err)...)
	}

	values := make(Values, len(in))
	for name, v := range in {
		values[name] = Value{Value: v.Value, Range: v.Range}
	}

	vars, res, moreDiags := e.evaluateModule(ctx, &Module{mod: mod, fsys: mod.FS}, values, req.Facts.Decode())
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	w, err := evalwire.EncodeValue(vars)
	if err != nil {
		return nil, append(diags, errorDiags("Evaluation not sent", err)...)
	}

	result, err := evalwire.EncodeResult(res)
	if err != nil {
		return nil, append(diags, errorDiags("Evaluation not sent", err)...)
	}

	return &evalwire.ModuleResponse{Vars: w, Result: result}, diags
}
