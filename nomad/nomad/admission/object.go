// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/parser"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Validation rules of custom object kinds (operator framework). They run in
// the same CEL environment as the admission policies, with these variables:
//
//   - self: the object being written ({"kind", "metadata", "spec",
//     "status"}),
//   - oldSelf: the stored object on update and null on create;
//     has(oldSelf) tests whether it exists,
//   - request: the operation ("create", "update" or "status"), the
//     namespace and the user, as for admission policies.
//
// The quantity() function converts a quantity such as "100GiB", "1.5Gi",
// "2k" or "500m" to a number, so that sizes can be compared.
const (
	VarSelf    = "self"
	VarOldSelf = "oldSelf"
)

// ObjectRuleEngine compiles and evaluates the CEL rules of object kinds.
// Compiled programs are cached per kind and modify index. It is safe for
// concurrent use.
type ObjectRuleEngine struct {
	// rules compiles the expressions in the object environment, with the
	// same output type check and cost limits as the admission policies.
	rules *PolicyEngine

	lock  sync.Mutex
	cache map[string]*compiledPolicy
}

// NewObjectRuleEngine returns a new object rule engine.
func NewObjectRuleEngine() (*ObjectRuleEngine, error) {
	env, err := cel.NewEnv(
		cel.ClearMacros(),
		cel.Macros(objectMacros()...),
		cel.Variable(VarSelf, cel.DynType),
		cel.Variable(VarOldSelf, cel.DynType),
		cel.Variable(VarRequest, cel.MapType(cel.StringType, cel.DynType)),
		cel.CrossTypeNumericComparisons(true),
		cel.DefaultUTCTimeZone(true),
		ext.Strings(),
		ext.Sets(),
		ext.Lists(),
		quantityFunction(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}
	return &ObjectRuleEngine{
		rules: &PolicyEngine{env: env, cache: make(map[string]*compiledPolicy)},
		cache: make(map[string]*compiledPolicy),
	}, nil
}

// objectMacros returns the standard macros with has() extended to accept
// the bare oldSelf variable: has(oldSelf) is true when an old object
// exists.
func objectMacros() []cel.Macro {
	hasOldSelf := parser.NewGlobalMacro(operators.Has, 1,
		func(eh parser.ExprHelper, target ast.Expr, args []ast.Expr) (ast.Expr, *common.Error) {
			if args[0].Kind() == ast.IdentKind && args[0].AsIdent() == VarOldSelf {
				return eh.NewCall(operators.NotEquals, eh.NewIdent(VarOldSelf), eh.NewLiteral(types.NullValue)), nil
			}
			return parser.MakeHas(eh, target, args)
		})

	macros := []cel.Macro{hasOldSelf}
	for _, m := range parser.AllMacros {
		if m.Function() != operators.Has {
			macros = append(macros, m)
		}
	}
	return macros
}

// quantityFunction declares quantity(), which converts a quantity to a
// double.
func quantityFunction() cel.EnvOption {
	return cel.Function("quantity",
		cel.Overload("quantity_string", []*cel.Type{cel.StringType}, cel.DoubleType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				q, err := ParseQuantity(string(v.(types.String)))
				if err != nil {
					return types.NewErr("%v", err)
				}
				return types.Double(q)
			})),
		cel.Overload("quantity_int", []*cel.Type{cel.IntType}, cel.DoubleType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				return types.Double(float64(v.(types.Int)))
			})),
		cel.Overload("quantity_double", []*cel.Type{cel.DoubleType}, cel.DoubleType,
			cel.UnaryBinding(func(v ref.Val) ref.Val {
				return v
			})),
	)
}

// quantitySuffixes are the multipliers of the quantity suffixes. A
// trailing "B" (bytes) is accepted after all of them except "m".
var quantitySuffixes = map[string]float64{
	"":   1,
	"m":  1e-3,
	"k":  1e3,
	"K":  1e3,
	"M":  1e6,
	"G":  1e9,
	"T":  1e12,
	"P":  1e15,
	"E":  1e18,
	"Ki": 1 << 10,
	"Mi": 1 << 20,
	"Gi": 1 << 30,
	"Ti": 1 << 40,
	"Pi": 1 << 50,
	"Ei": 1 << 60,
}

// ParseQuantity parses a quantity: a decimal number with an optional SI
// ("k", "M", "G", ...), binary ("Ki", "Mi", "Gi", ...) or milli ("m")
// suffix, optionally followed by "B" ("100GiB", "1.5G", "500m").
func ParseQuantity(s string) (float64, error) {
	in := strings.TrimSpace(s)
	end := len(in)
	for end > 0 && !(in[end-1] >= '0' && in[end-1] <= '9' || in[end-1] == '.') {
		end--
	}
	number, suffix := in[:end], in[end:]
	if unit, ok := strings.CutSuffix(suffix, "B"); ok {
		if unit == "m" {
			return 0, fmt.Errorf("invalid quantity %q", s)
		}
		suffix = unit
	}
	mult, ok := quantitySuffixes[suffix]
	if !ok || number == "" {
		return 0, fmt.Errorf("invalid quantity %q", s)
	}
	f, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, fmt.Errorf("invalid quantity %q", s)
	}
	return f * mult, nil
}

// CompileRules compiles all the rules of the kind and returns an error that
// lists every rule that does not compile.
func (e *ObjectRuleEngine) CompileRules(kind *structs.ObjectKind) error {
	_, err := e.compile(kind)
	return err
}

func (e *ObjectRuleEngine) compile(kind *structs.ObjectKind) ([]cel.Program, error) {
	// The kind's rules have the shape of an admission policy's rules.
	return e.rules.compile(&structs.AdmissionPolicy{Rules: objectRulesAsPolicyRules(kind.Rules)})
}

func objectRulesAsPolicyRules(rules []*structs.ObjectKindRule) []*structs.AdmissionPolicyRule {
	out := make([]*structs.AdmissionPolicyRule, len(rules))
	for i, r := range rules {
		if r != nil {
			out[i] = &structs.AdmissionPolicyRule{Expression: r.Expression, Message: r.Message}
		}
	}
	return out
}

// programs returns the cached programs of the kind, compiling them if the
// kind changed since they were cached.
func (e *ObjectRuleEngine) programs(kind *structs.ObjectKind) ([]cel.Program, error) {
	key := kind.KindRef()
	e.lock.Lock()
	cached, ok := e.cache[key]
	e.lock.Unlock()
	if ok && cached.modifyIndex == kind.ModifyIndex && kind.ModifyIndex != 0 {
		return cached.programs, cached.err
	}

	programs, err := e.compile(kind)
	if kind.ModifyIndex != 0 {
		e.lock.Lock()
		e.cache[key] = &compiledPolicy{modifyIndex: kind.ModifyIndex, programs: programs, err: err}
		e.lock.Unlock()
	}
	return programs, err
}

// Forget removes the cached programs of a kind.
func (e *ObjectRuleEngine) Forget(kindRef string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	delete(e.cache, kindRef)
}

// Evaluate evaluates the rules of the kind and returns every violation. A
// rule that fails to evaluate counts as violated, so a broken rule fails
// closed. oldSelf is nil on create.
func (e *ObjectRuleEngine) Evaluate(ctx context.Context, kind *structs.ObjectKind, self, oldSelf any, request map[string]any) []*Violation {
	if len(kind.Rules) == 0 {
		return nil
	}
	programs, compileErr := e.programs(kind)
	return evaluateObjectRules(ctx, kind, programs, compileErr, self, oldSelf, request)
}

// PreparedRules are the compiled rules of a kind that is not stored (yet),
// for evaluating them on many objects.
type PreparedRules struct {
	kind       *structs.ObjectKind
	programs   []cel.Program
	compileErr error
}

// Prepare compiles the rules of a kind without caching them.
func (e *ObjectRuleEngine) Prepare(kind *structs.ObjectKind) *PreparedRules {
	programs, err := e.compile(kind)
	return &PreparedRules{kind: kind, programs: programs, compileErr: err}
}

// Evaluate evaluates the prepared rules like ObjectRuleEngine.Evaluate.
func (p *PreparedRules) Evaluate(ctx context.Context, self, oldSelf any, request map[string]any) []*Violation {
	if len(p.kind.Rules) == 0 {
		return nil
	}
	return evaluateObjectRules(ctx, p.kind, p.programs, p.compileErr, self, oldSelf, request)
}

func evaluateObjectRules(ctx context.Context, kind *structs.ObjectKind, programs []cel.Program, compileErr error,
	self, oldSelf any, request map[string]any) []*Violation {

	ctx, cancel := context.WithTimeout(ctx, celEvalTimeout)
	defer cancel()

	vars := map[string]any{
		VarSelf:    self,
		VarOldSelf: oldSelf,
		VarRequest: request,
	}
	if oldSelf == nil {
		vars[VarOldSelf] = types.NullValue
	}

	var violations []*Violation
	for i, rule := range kind.Rules {
		violation := func(msg string) {
			violations = append(violations, &Violation{
				Policy:  kind.KindRef(),
				Action:  structs.AdmissionPolicyActionDeny,
				Rule:    i,
				Message: msg,
			})
		}

		var prg cel.Program
		if i < len(programs) {
			prg = programs[i]
		}
		if prg == nil {
			violation(fmt.Sprintf("rule %d does not compile: %v", i, compileErr))
			continue
		}

		out, _, err := prg.ContextEval(ctx, vars)
		if err != nil {
			violation(fmt.Sprintf("rule %d failed to evaluate: %v", i, err))
			continue
		}
		ok, isBool := out.Value().(bool)
		switch {
		case !isBool:
			violation(fmt.Sprintf("rule %d did not evaluate to a bool", i))
		case !ok && rule.Message != "":
			violation(rule.Message)
		case !ok:
			violation(fmt.Sprintf("rule %d failed: %s", i, rule.Expression))
		}
	}
	return violations
}

// ObjectValue converts a JSON-like value into the generic value CEL rules
// see. Integral numbers become integers, so rules can compare them with
// integer literals.
func ObjectValue(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out, err := decodeJSON(b)
	if err != nil {
		return nil, err
	}
	return normalizeNumbers(out), nil
}
