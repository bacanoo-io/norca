// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

// metricFamilies is the naming table of MetricsServiceName as data: the
// first element of every go-metrics key in the module and the directories
// (relative to the module root) whose code may emit it. The library
// families (runtime, memberlist, serf and the Raft library's own keys) are
// not emitted by the module's code and are checked end to end by the
// agent's OTLP export test.
var metricFamilies = map[string][]string{
	"server":     {"norca/", "lib/auth/jwt/"},
	"client":     {"client/", "command/agent/consul/"},
	"raft":       {"norca/"},
	"state":      {"norca/"},
	"scheduler":  {"scheduler/"},
	"autoscaler": {"norca/autoscaler/"},
	"badger":     {"helper/badgerdb/"},
	"agent":      {"command/agent/", "helper/telemetry/"},
	"http":       {"command/agent/"},
}

// goMetricsFuncs are the go-metrics functions that take a key first.
var goMetricsFuncs = []string{
	"AddSample", "AddSampleWithLabels",
	"EmitKey",
	"IncrCounter", "IncrCounterWithLabels",
	"MeasureSince", "MeasureSinceWithLabels",
	"SetGauge", "SetGaugeWithLabels",
	"SetPrecisionGauge", "SetPrecisionGaugeWithLabels",
}

// TestMetricsServiceName pins the service name every metric starts with.
func TestMetricsServiceName(t *testing.T) {
	ci.Parallel(t)
	must.Eq(t, "norca", MetricsServiceName)
}

// TestMetricFamilies reads every go-metrics call outside the tests of the
// module and checks the first element of its key against the naming table:
// no key may start with "nomad", every family is in the table, and only the
// listed directories emit it. A key the test cannot read (not a []string
// literal, an append to one, or a variable of the same file holding one)
// fails too, so every name stays visible in the source.
func TestMetricFamilies(t *testing.T) {
	ci.Parallel(t)

	root, err := filepath.Abs(filepath.Join("..", ".."))
	must.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	must.NoError(t, err, must.Sprint("the test runs in helper/telemetry of the module"))

	seen := map[string]int{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "ui", "website", "node_modules", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, key := range metricKeysOf(t, path) {
			seen[key.family]++
			checkFamily(t, rel, key)
		}
		return nil
	})
	must.NoError(t, err)

	// The table has no stale rows.
	for family := range metricFamilies {
		must.Positive(t, seen[family], must.Sprintf("no key of the family %q is left; remove it from the table", family))
	}
}

func checkFamily(t *testing.T, rel string, key metricKey) {
	t.Helper()
	where := rel + ":" + strconv.Itoa(key.line)
	if key.family == "" {
		t.Errorf("%s: the key of the go-metrics call cannot be read; use a []string literal", where)
		return
	}
	dirs, ok := metricFamilies[key.family]
	if !ok {
		t.Errorf("%s: the family %q is not in the naming table (helper/telemetry/names.go)", where, key.family)
		return
	}
	if !slices.ContainsFunc(dirs, func(dir string) bool { return strings.HasPrefix(rel, dir) }) {
		t.Errorf("%s: the family %q is emitted outside %v", where, key.family, dirs)
	}
}

// metricKey is the first element of the key of one go-metrics call.
type metricKey struct {
	line   int
	family string
}

// metricKeysOf returns the keys of the go-metrics calls of a file.
func metricKeysOf(t *testing.T, path string) []metricKey {
	t.Helper()
	src, err := os.ReadFile(path)
	must.NoError(t, err)
	if !bytes.Contains(src, []byte("/go-metrics\"")) {
		return nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	must.NoError(t, err)

	pkgName := ""
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != "github.com/hashicorp/go-metrics" && p != "github.com/armon/go-metrics" {
			continue
		}
		pkgName = "metrics"
		if imp.Name != nil {
			pkgName = imp.Name.Name
		}
	}
	if pkgName == "" || pkgName == "_" {
		return nil
	}

	vars := keyVariables(file)
	var keys []metricKey
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !slices.Contains(goMetricsFuncs, sel.Sel.Name) {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != pkgName {
			return true
		}
		line := fset.Position(call.Pos()).Line
		families := firstElements(call.Args[0], vars, 0)
		if len(families) == 0 {
			keys = append(keys, metricKey{line: line})
		}
		for _, family := range families {
			keys = append(keys, metricKey{line: line, family: family})
		}
		return true
	})
	return keys
}

// keyVariables maps the names of a file's variables to the values assigned
// to them, ignoring scopes: enough for the few keys held in variables.
func keyVariables(file *ast.File) map[string][]ast.Expr {
	vars := map[string][]ast.Expr{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if i < len(n.Values) {
					vars[name.Name] = append(vars[name.Name], n.Values[i])
				}
			}
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				return true
			}
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok {
					vars[id.Name] = append(vars[id.Name], n.Rhs[i])
				}
			}
		}
		return true
	})
	return vars
}

// firstElements returns the first element of the []string the expression
// evaluates to, or nothing when it cannot be read.
func firstElements(e ast.Expr, vars map[string][]ast.Expr, depth int) []string {
	if depth > 3 {
		return nil
	}
	switch e := e.(type) {
	case *ast.CompositeLit:
		if len(e.Elts) == 0 {
			return nil
		}
		lit, ok := e.Elts[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil
		}
		return []string{s}
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok && fn.Name == "append" && len(e.Args) > 0 {
			return firstElements(e.Args[0], vars, depth+1)
		}
	case *ast.Ident:
		var out []string
		for _, v := range vars[e.Name] {
			out = append(out, firstElements(v, vars, depth+1)...)
		}
		return out
	}
	return nil
}
