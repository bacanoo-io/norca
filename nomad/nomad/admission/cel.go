// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// celCostLimit bounds the runtime cost of a single rule evaluation.
	celCostLimit = 1_000_000

	// celEvalTimeout bounds the wall clock time of the evaluation of all
	// policies for a single job.
	celEvalTimeout = 2 * time.Second

	// VarJob and VarRequest are the variables available to CEL rules.
	VarJob     = "job"
	VarRequest = "request"

	// VarVariable and VarOldVariable are the variables available to CEL
	// rules of Variables writes: the written Variable and the Variable it
	// replaces or deletes (null if there is none). They are null when a job
	// is admitted, and VarJob is null when a Variables write is admitted.
	VarVariable    = "variable"
	VarOldVariable = "oldVariable"

	// VarRelease is the variable available to CEL rules of pack releases:
	// the release with its pack (name, version, registry, digest, signer,
	// signed). It is null when jobs or Variables writes are admitted.
	VarRelease = "release"
)

// Violation is a rule of an admission policy that a job does not satisfy.
type Violation struct {
	// Policy is the name of the admission policy.
	Policy string

	// Action is the action of the policy: deny or warn.
	Action string

	// Rule is the index of the violated rule.
	Rule int

	// Message describes the violation.
	Message string
}

func (v *Violation) Error() string {
	return v.Message
}

// compiledPolicy caches the programs of a policy at a given modify index.
type compiledPolicy struct {
	modifyIndex uint64
	programs    []cel.Program
	err         error
}

// PolicyEngine compiles and evaluates CEL admission policies. Compiled
// programs are cached per policy and modify index. It is safe for
// concurrent use.
type PolicyEngine struct {
	env *cel.Env

	lock  sync.Mutex
	cache map[string]*compiledPolicy
}

// NewPolicyEngine returns a new policy engine.
func NewPolicyEngine() (*PolicyEngine, error) {
	env, err := cel.NewEnv(
		cel.Variable(VarJob, cel.DynType),
		cel.Variable(VarRequest, cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable(VarVariable, cel.DynType),
		cel.Variable(VarOldVariable, cel.DynType),
		cel.Variable(VarRelease, cel.DynType),
		cel.CrossTypeNumericComparisons(true),
		cel.DefaultUTCTimeZone(true),
		ext.Strings(),
		ext.Sets(),
		ext.Lists(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}
	return &PolicyEngine{
		env:   env,
		cache: make(map[string]*compiledPolicy),
	}, nil
}

// compileRule compiles a single CEL expression into a program. The
// expression must evaluate to a bool.
func (e *PolicyEngine) compileRule(expr string) (cel.Program, error) {
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	out := ast.OutputType()
	if !out.IsExactType(cel.BoolType) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression must evaluate to a bool, not %s", out)
	}
	return e.env.Program(ast,
		cel.EvalOptions(cel.OptOptimize),
		cel.CostLimit(celCostLimit),
		cel.InterruptCheckFrequency(100),
	)
}

// Compile compiles all the rules of the policy and returns an error that
// lists every rule that does not compile.
func (e *PolicyEngine) Compile(policy *structs.AdmissionPolicy) error {
	_, err := e.compile(policy)
	return err
}

func (e *PolicyEngine) compile(policy *structs.AdmissionPolicy) ([]cel.Program, error) {
	var mErr *multierror.Error
	programs := make([]cel.Program, len(policy.Rules))
	for i, rule := range policy.Rules {
		if rule == nil {
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: missing expression", i))
			continue
		}
		prg, err := e.compileRule(rule.Expression)
		if err != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("rule %d: %v", i, err))
			continue
		}
		programs[i] = prg
	}
	return programs, mErr.ErrorOrNil()
}

// programs returns the cached programs of the policy, compiling it if it
// changed since it was cached.
func (e *PolicyEngine) programs(policy *structs.AdmissionPolicy) ([]cel.Program, error) {
	e.lock.Lock()
	cached, ok := e.cache[policy.Name]
	e.lock.Unlock()
	if ok && cached.modifyIndex == policy.ModifyIndex && policy.ModifyIndex != 0 {
		return cached.programs, cached.err
	}

	programs, err := e.compile(policy)
	e.lock.Lock()
	e.cache[policy.Name] = &compiledPolicy{
		modifyIndex: policy.ModifyIndex,
		programs:    programs,
		err:         err,
	}
	e.lock.Unlock()
	return programs, err
}

// Prune removes cached programs of policies that no longer exist.
func (e *PolicyEngine) Prune(existing map[string]struct{}) {
	e.lock.Lock()
	defer e.lock.Unlock()
	for name := range e.cache {
		if _, ok := existing[name]; !ok {
			delete(e.cache, name)
		}
	}
}

// Evaluate evaluates the given policies against the job and the request
// variables and returns every violation. A rule that fails to evaluate
// (e.g. accesses a missing field without has()) counts as violated, so a
// broken deny rule fails closed.
func (e *PolicyEngine) Evaluate(ctx context.Context, policies []*structs.AdmissionPolicy, job any, request map[string]any) []*Violation {
	return e.evaluate(ctx, policies, map[string]any{
		VarJob:         job,
		VarRequest:     request,
		VarVariable:    nil,
		VarOldVariable: nil,
		VarRelease:     nil,
	})
}

// EvaluateVariable evaluates the given policies against a Variables write
// and returns every violation, like Evaluate. variable and oldVariable are
// usually built with VariableInput.Value; a nil oldVariable is null in
// CEL.
func (e *PolicyEngine) EvaluateVariable(ctx context.Context, policies []*structs.AdmissionPolicy,
	variable, oldVariable map[string]any, request map[string]any) []*Violation {

	vars := map[string]any{
		VarJob:         nil,
		VarRequest:     request,
		VarVariable:    variable,
		VarOldVariable: nil,
		VarRelease:     nil,
	}
	if oldVariable != nil {
		vars[VarOldVariable] = oldVariable
	}
	return e.evaluate(ctx, policies, vars)
}

// EvaluateRelease evaluates the CEL rules of the given policies against a
// pack release and returns every violation, like Evaluate.
func (e *PolicyEngine) EvaluateRelease(ctx context.Context, policies []*structs.AdmissionPolicy,
	release map[string]any, request map[string]any) []*Violation {

	return e.evaluate(ctx, policies, map[string]any{
		VarJob:         nil,
		VarRequest:     request,
		VarVariable:    nil,
		VarOldVariable: nil,
		VarRelease:     release,
	})
}

func (e *PolicyEngine) evaluate(ctx context.Context, policies []*structs.AdmissionPolicy, vars map[string]any) []*Violation {
	if len(policies) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, celEvalTimeout)
	defer cancel()

	var violations []*Violation
	for _, policy := range policies {
		programs, compileErr := e.programs(policy)
		for i, rule := range policy.Rules {
			violation := func(msg string) {
				violations = append(violations, &Violation{
					Policy:  policy.Name,
					Action:  policy.Action,
					Rule:    i,
					Message: msg,
				})
			}

			prg := programs[i]
			if prg == nil {
				violation(fmt.Sprintf("admission policy %q rule %d does not compile: %v", policy.Name, i, compileErr))
				continue
			}

			out, _, err := prg.ContextEval(ctx, vars)
			if err != nil {
				violation(fmt.Sprintf("admission policy %q rule %d failed to evaluate: %v", policy.Name, i, err))
				continue
			}
			ok, isBool := out.Value().(bool)
			if !isBool {
				violation(fmt.Sprintf("admission policy %q rule %d did not evaluate to a bool", policy.Name, i))
				continue
			}
			if ok {
				continue
			}

			msg := rule.Message
			if msg == "" {
				msg = fmt.Sprintf("rule %d failed: %s", i, rule.Expression)
			}
			violation(fmt.Sprintf("admission policy %q: %s", policy.Name, msg))
		}
	}
	return violations
}
