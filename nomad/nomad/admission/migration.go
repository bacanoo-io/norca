// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"
	"github.com/hashicorp/nomad/nomad/structs"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// Migrations of custom objects (operator framework phase 4): when the schema
// version of a kind is raised, the CEL expressions of its migration convert
// the stored objects. They see:
//
//   - self: the stored object, as the rules see it ({"kind", "metadata",
//     "spec", "status"}),
//   - fromVersion: the schema version the object is stored at,
//   - toVersion: the new schema version of the kind,
//
// and return the new spec (or status) as a map. Maps have two helpers for
// this: m.merge(other) returns m with the entries of other added or
// replaced, m.omit(["a", "b"]) returns m without the keys a and b. For
// example, renaming the field "port" to a list "ports":
//
//	self.spec.omit(["port"]).merge({"ports": [self.spec.port]})
const (
	VarFromVersion = "fromVersion"
	VarToVersion   = "toVersion"
)

// MigrationEngine compiles and evaluates migration expressions. Compiled
// programs are cached per kind and modify index. It is safe for concurrent
// use.
type MigrationEngine struct {
	env *cel.Env

	lock  sync.Mutex
	cache map[string]*compiledMigration
}

type compiledMigration struct {
	modifyIndex uint64
	prepared    *PreparedMigration
	err         error
}

// NewMigrationEngine returns a new migration engine.
func NewMigrationEngine() (*MigrationEngine, error) {
	env, err := cel.NewEnv(
		cel.Variable(VarSelf, cel.DynType),
		cel.Variable(VarFromVersion, cel.IntType),
		cel.Variable(VarToVersion, cel.IntType),
		cel.CrossTypeNumericComparisons(true),
		cel.DefaultUTCTimeZone(true),
		ext.Strings(),
		ext.Sets(),
		ext.Lists(),
		quantityFunction(),
		mapHelperFunctions(),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}
	return &MigrationEngine{env: env, cache: make(map[string]*compiledMigration)}, nil
}

var dynMapType = cel.MapType(cel.DynType, cel.DynType)

// mapHelperFunctions declares m.merge(other) and m.omit(keys).
func mapHelperFunctions() cel.EnvOption {
	return cel.Lib(mapHelpersLib{})
}

type mapHelpersLib struct{}

func (mapHelpersLib) LibraryName() string { return "norca.migration.maps" }

func (mapHelpersLib) ProgramOptions() []cel.ProgramOption { return nil }

func (mapHelpersLib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("merge",
			cel.MemberOverload("map_merge_map", []*cel.Type{dynMapType, dynMapType}, dynMapType,
				cel.BinaryBinding(func(a, b ref.Val) ref.Val {
					am, ok1 := a.(traits.Mapper)
					bm, ok2 := b.(traits.Mapper)
					if !ok1 || !ok2 {
						return types.NewErr("merge() requires two maps")
					}
					out := map[ref.Val]ref.Val{}
					copyEntries(out, am, nil)
					copyEntries(out, bm, nil)
					return types.NewRefValMap(types.DefaultTypeAdapter, out)
				}))),
		cel.Function("omit",
			cel.MemberOverload("map_omit_list", []*cel.Type{dynMapType, cel.ListType(cel.DynType)}, dynMapType,
				cel.BinaryBinding(func(a, b ref.Val) ref.Val {
					am, ok1 := a.(traits.Mapper)
					keys, ok2 := b.(traits.Lister)
					if !ok1 || !ok2 {
						return types.NewErr("omit() requires a map and a list")
					}
					out := map[ref.Val]ref.Val{}
					copyEntries(out, am, keys)
					return types.NewRefValMap(types.DefaultTypeAdapter, out)
				}))),
	}
}

// copyEntries copies the entries of m to out, except the keys in skip.
func copyEntries(out map[ref.Val]ref.Val, m traits.Mapper, skip traits.Lister) {
	it := m.Iterator()
	for it.HasNext() == types.True {
		k := it.Next()
		if skip != nil && skip.Contains(k) == types.True {
			continue
		}
		// Replace an equal key of another type representation.
		for existing := range out {
			if existing.Equal(k) == types.True {
				delete(out, existing)
			}
		}
		out[k] = m.Get(k)
	}
}

// compileExpr compiles a migration expression, which must return a map.
func (e *MigrationEngine) compileExpr(expr string) (cel.Program, error) {
	ast, iss := e.env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	out := ast.OutputType()
	if !dynMapType.IsAssignableType(out) && !out.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("expression must evaluate to a map, not %s", out)
	}
	return e.env.Program(ast,
		cel.EvalOptions(cel.OptOptimize),
		cel.CostLimit(celCostLimit),
		cel.InterruptCheckFrequency(100),
	)
}

// PreparedMigration is the compiled migration of a kind.
type PreparedMigration struct {
	spec      cel.Program
	status    cel.Program
	toVersion uint64
}

// Compile compiles the migration of the kind and returns an error that
// names every expression that does not compile.
func (e *MigrationEngine) Compile(m *structs.ObjectKindMigration) error {
	_, err := e.Prepare(m, 1)
	return err
}

// Prepare compiles a migration to the schema version toVersion. A nil
// migration gives a migration that changes nothing.
func (e *MigrationEngine) Prepare(m *structs.ObjectKindMigration, toVersion uint64) (*PreparedMigration, error) {
	p := &PreparedMigration{toVersion: toVersion}
	if m == nil {
		return p, nil
	}
	var errs []error
	var err error
	if m.Spec != "" {
		if p.spec, err = e.compileExpr(m.Spec); err != nil {
			errs = append(errs, fmt.Errorf("spec: %v", err))
		}
	}
	if m.Status != "" {
		if p.status, err = e.compileExpr(m.Status); err != nil {
			errs = append(errs, fmt.Errorf("status: %v", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return p, nil
}

// ForKind returns the compiled migration of a stored kind, cached per
// modify index.
func (e *MigrationEngine) ForKind(kind *structs.ObjectKind) (*PreparedMigration, error) {
	key := kind.KindRef()
	e.lock.Lock()
	cached, ok := e.cache[key]
	e.lock.Unlock()
	if ok && cached.modifyIndex == kind.ModifyIndex && kind.ModifyIndex != 0 {
		return cached.prepared, cached.err
	}
	p, err := e.Prepare(kind.Migration, kind.SchemaVersion())
	if kind.ModifyIndex != 0 {
		e.lock.Lock()
		e.cache[key] = &compiledMigration{modifyIndex: kind.ModifyIndex, prepared: p, err: err}
		e.lock.Unlock()
	}
	return p, err
}

// Forget removes the cached programs of a kind.
func (e *MigrationEngine) Forget(kindRef string) {
	e.lock.Lock()
	defer e.lock.Unlock()
	delete(e.cache, kindRef)
}

// Migrate runs the migration on an object. self is the object as the rules
// see it (see ObjectValue). It returns the new spec and status; a part
// without an expression is returned unchanged.
func (p *PreparedMigration) Migrate(ctx context.Context, self any, fromVersion uint64,
	spec, status map[string]any) (map[string]any, map[string]any, error) {

	if p.spec == nil && p.status == nil {
		return spec, status, nil
	}
	ctx, cancel := context.WithTimeout(ctx, celEvalTimeout)
	defer cancel()
	vars := map[string]any{
		VarSelf:        self,
		VarFromVersion: int64(fromVersion),
		VarToVersion:   int64(p.toVersion),
	}
	run := func(part string, prg cel.Program, current map[string]any) (map[string]any, error) {
		if prg == nil {
			return current, nil
		}
		out, _, err := prg.ContextEval(ctx, vars)
		if err != nil {
			return nil, fmt.Errorf("%s migration failed: %v", part, err)
		}
		m, err := celMapToGo(out)
		if err != nil {
			return nil, fmt.Errorf("%s migration: %v", part, err)
		}
		return m, nil
	}
	newSpec, err := run("spec", p.spec, spec)
	if err != nil {
		return nil, nil, err
	}
	newStatus, err := run("status", p.status, status)
	if err != nil {
		return nil, nil, err
	}
	return newSpec, newStatus, nil
}

// celMapToGo converts a CEL map to the JSON-like Go form of objects.
func celMapToGo(v ref.Val) (map[string]any, error) {
	if _, ok := v.(traits.Mapper); !ok {
		return nil, fmt.Errorf("result is a %s, not a map", v.Type().TypeName())
	}
	native, err := v.ConvertToNative(reflect.TypeOf(&structpb.Value{}))
	if err != nil {
		return nil, fmt.Errorf("result can't be converted to JSON: %v", err)
	}
	b, err := protojson.Marshal(native.(*structpb.Value))
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
