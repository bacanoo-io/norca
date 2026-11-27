// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package logcheck is an analyzer of the hclog calls of the norca module: it
// reports what breaks the log convention of the agent (convention.go).
//
// For a call of Trace, Debug, Info, Warn, Error or Log on an hclog.Logger it
// reports a message that is not a constant (a message built with
// fmt.Sprintf, concatenated or taken from a variable), a message that starts
// with a capital letter followed by a lower case one, ends with a period or
// holds a format verb; for those calls and for With it reports an odd number
// of key/value arguments, a key that is not a constant, not lower
// snake_case, replaced by a key of the convention, ambiguous or reserved,
// an error logged under another key than error (or a key ending in _error),
// an object of the structs package logged as a whole where its ID is meant,
// and a secret field logged as a value.
//
// A message may be a parameter of the enclosing function or the result of
// fmt.Sprintf with a format that is not a constant: that is an adapter
// forwarding the messages of another logging API. A call with the comment
// //logcheck:ignore on its line or the line above it is not checked (for
// what the rules can't see: a message forwarded from a plugin, say).
// Generated files are not checked.
//
// Run it with go vet:
//
//	go -C tools build -o /tmp/logcheck ./logcheck/cmd/logcheck
//	go vet -vettool=/tmp/logcheck ./...
//
// or on its own (logcheck ./...), where -fix applies the suggested fixes.
package logcheck

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer reports the hclog calls that break the log convention.
var Analyzer = &analysis.Analyzer{
	Name:     "logcheck",
	Doc:      "check that hclog calls follow the log convention of the agent (keys, constant messages)",
	URL:      "https://github.com/bacanoo-io/norca/tree/main/norca/tools/logcheck",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const hclogPath = "github.com/hashicorp/go-hclog"

// msgIndex maps the logging methods of hclog.Logger to the index of their
// message argument; With has none (-1).
var msgIndex = map[string]int{
	"Trace": 0, "Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"Log":  1,
	"With": -1,
}

var (
	snakeKey   = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	formatVerb = regexp.MustCompile(`%[-+# 0]*[0-9]*(\.[0-9]+)?[vsdqxXtfgeTp]`)
)

func run(pass *analysis.Pass) (any, error) {
	ins, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		return nil, fmt.Errorf("logcheck: no inspector result")
	}
	c := &checker{pass: pass, logger: findLogger(pass.Pkg)}
	skip := map[*token.File]bool{}
	for _, f := range pass.Files {
		tf := pass.Fset.File(f.Pos())
		if ast.IsGenerated(f) {
			skip[tf] = true
			continue
		}
		c.collectParams(f)
		c.collectIgnores(f)
	}
	ins.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok || skip[pass.Fset.File(call.Pos())] {
			return
		}
		c.checkCall(call)
	})
	return nil, nil
}

type checker struct {
	pass   *analysis.Pass
	logger *types.Interface
	// params holds the parameters of the functions of the package: a
	// message that is one is forwarded.
	params map[types.Object]bool
	// ignored holds the lines marked with //logcheck:ignore, per file.
	ignored map[*token.File]map[int]bool
}

func (c *checker) collectParams(f *ast.File) {
	if c.params == nil {
		c.params = map[types.Object]bool{}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		var ft *ast.FuncType
		switch fn := n.(type) {
		case *ast.FuncDecl:
			ft = fn.Type
		case *ast.FuncLit:
			ft = fn.Type
		default:
			return true
		}
		for _, field := range ft.Params.List {
			for _, name := range field.Names {
				if obj := c.pass.TypesInfo.Defs[name]; obj != nil {
					c.params[obj] = true
				}
			}
		}
		return true
	})
}

func (c *checker) collectIgnores(f *ast.File) {
	if c.ignored == nil {
		c.ignored = map[*token.File]map[int]bool{}
	}
	tf := c.pass.Fset.File(f.Pos())
	for _, cg := range f.Comments {
		for _, cm := range cg.List {
			if strings.HasPrefix(cm.Text, "//logcheck:ignore") {
				if c.ignored[tf] == nil {
					c.ignored[tf] = map[int]bool{}
				}
				c.ignored[tf][tf.Line(cm.Pos())] = true
			}
		}
	}
}

func (c *checker) isIgnored(call *ast.CallExpr) bool {
	tf := c.pass.Fset.File(call.Pos())
	line := tf.Line(call.Pos())
	return c.ignored[tf][line] || c.ignored[tf][line-1]
}

// findLogger returns the hclog.Logger interface if the package imports
// hclog, directly or through its imports.
func findLogger(pkg *types.Package) *types.Interface {
	seen := map[*types.Package]bool{}
	var find func(p *types.Package) *types.Interface
	find = func(p *types.Package) *types.Interface {
		if p == nil || seen[p] {
			return nil
		}
		seen[p] = true
		if p.Path() == hclogPath {
			if obj := p.Scope().Lookup("Logger"); obj != nil {
				if iface, ok := obj.Type().Underlying().(*types.Interface); ok {
					return iface
				}
			}
			return nil
		}
		for _, imp := range p.Imports() {
			if iface := find(imp); iface != nil {
				return iface
			}
		}
		return nil
	}
	return find(pkg)
}

// logMethod returns the name of the hclog.Logger method the call calls, or
// "" if it calls none.
func (c *checker) logMethod(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if _, ok := msgIndex[sel.Sel.Name]; !ok {
		return ""
	}
	s := c.pass.TypesInfo.Selections[sel]
	if s == nil || s.Kind() != types.MethodVal {
		return ""
	}
	fn, ok := s.Obj().(*types.Func)
	if !ok {
		return ""
	}
	if fn.Pkg() != nil && fn.Pkg().Path() == hclogPath {
		return sel.Sel.Name
	}
	if c.logger != nil && (types.Implements(s.Recv(), c.logger) || types.Implements(types.NewPointer(s.Recv()), c.logger)) {
		return sel.Sel.Name
	}
	return ""
}

func (c *checker) checkCall(call *ast.CallExpr) {
	method := c.logMethod(call)
	if method == "" || c.isIgnored(call) {
		return
	}
	args := call.Args
	if idx := msgIndex[method]; idx >= 0 {
		if len(args) <= idx {
			return
		}
		c.checkMessage(args[idx])
		args = args[idx+1:]
	}
	if call.Ellipsis != token.NoPos {
		// The key/value pairs are built elsewhere.
		return
	}
	if len(args)%2 != 0 && !c.isStacktrace(args[len(args)-1]) {
		c.pass.Reportf(call.Lparen, "odd number of key/value arguments (%d)", len(args))
	}
	for i := 0; i+1 < len(args); i += 2 {
		c.checkPair(args[i], args[i+1])
	}
}

// isStacktrace reports whether e is an hclog.CapturedStacktrace, which hclog
// takes as a last argument without a key.
func (c *checker) isStacktrace(e ast.Expr) bool {
	return isNamed(c.pass.TypesInfo.TypeOf(e), hclogPath, "CapturedStacktrace")
}

func (c *checker) checkMessage(msg ast.Expr) {
	tv := c.pass.TypesInfo.Types[msg]
	if tv.Value != nil {
		if tv.Value.Kind() == constant.String {
			c.checkMessageText(msg, constant.StringVal(tv.Value))
		}
		return
	}
	if id, ok := ast.Unparen(msg).(*ast.Ident); ok && c.params[c.pass.TypesInfo.Uses[id]] {
		return // forwarded
	}
	if call, ok := ast.Unparen(msg).(*ast.CallExpr); ok && c.isFmtCall(call) {
		if len(call.Args) > 0 && c.pass.TypesInfo.Types[call.Args[0]].Value == nil {
			return // a printf-style API forwarded
		}
		c.pass.Reportf(msg.Pos(), "message built with fmt: use a constant message and key/value pairs")
		return
	}
	c.pass.Reportf(msg.Pos(), "message is not a constant: use a constant message and key/value pairs")
}

func (c *checker) isFmtCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := c.pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	return ok && fn.Pkg() != nil && fn.Pkg().Path() == "fmt" && strings.HasPrefix(fn.Name(), "Sprint")
}

func (c *checker) checkMessageText(msg ast.Expr, text string) {
	lit, isLit := ast.Unparen(msg).(*ast.BasicLit)
	fixed := text
	var problems []string
	if strings.HasSuffix(text, ".") && !strings.HasSuffix(text, "..") {
		fixed = strings.TrimSuffix(fixed, ".")
		problems = append(problems, "ends with a period")
	}
	if r, size := utf8.DecodeRuneInString(text); unicode.IsUpper(r) && !isIdentifier(firstWord(text)) {
		if next, _ := utf8.DecodeRuneInString(text[size:]); unicode.IsLower(next) {
			fixed = string(unicode.ToLower(r)) + fixed[size:]
			problems = append(problems, "starts with a capital letter")
		}
	}
	if formatVerb.MatchString(text) {
		c.pass.Reportf(msg.Pos(), "message holds a format verb: hclog does not format messages; use key/value pairs")
	}
	if len(problems) == 0 {
		return
	}
	d := analysis.Diagnostic{
		Pos:     msg.Pos(),
		Message: "message " + strings.Join(problems, " and ") + ": write it in lower case without a trailing period",
	}
	if isLit && lit.Kind == token.STRING {
		d.SuggestedFixes = []analysis.SuggestedFix{{
			Message:   "rewrite the message",
			TextEdits: []analysis.TextEdit{{Pos: lit.Pos(), End: lit.End(), NewText: []byte(strconv.Quote(fixed))}},
		}}
	}
	c.pass.Report(d)
}

// firstWord returns the text up to the first space or punctuation.
func firstWord(text string) string {
	if i := strings.IndexAny(text, " :,;.()"); i >= 0 {
		return text[:i]
	}
	return text
}

// isIdentifier reports whether a word is written like a Go identifier or
// an acronym (UpsertJob, IPv4): an upper case letter after its first one.
func isIdentifier(word string) bool {
	for i, r := range word {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func (c *checker) checkPair(keyExpr, value ast.Expr) {
	tv := c.pass.TypesInfo.Types[keyExpr]
	if tv.Value == nil || tv.Value.Kind() != constant.String {
		c.pass.Reportf(keyExpr.Pos(), "key is not a constant string")
		return
	}
	key := c.checkKey(keyExpr, constant.StringVal(tv.Value))
	c.checkValue(keyExpr, key, value)
}

// checkKey reports a key that breaks the convention and returns the key the
// convention wants.
func (c *checker) checkKey(keyExpr ast.Expr, key string) string {
	want := key
	var why string
	if !snakeKey.MatchString(key) {
		want = snakeCase(key)
		why = "is not lower snake_case"
	}
	if r, ok := renamed[want]; ok {
		want = r
		why = "is not the key of the convention"
	}
	if want != key {
		c.reportKey(keyExpr, fmt.Sprintf("key %q %s: use %q", key, why, want), want)
		return want
	}
	if advice, ok := ambiguous[key]; ok {
		c.pass.Reportf(keyExpr.Pos(), "key %q is ambiguous: %s", key, advice)
		return key
	}
	if pkg, ok := reserved[key]; ok && !strings.HasSuffix(c.pass.Pkg.Path(), "/"+pkg) {
		c.pass.Reportf(keyExpr.Pos(), "key %q is reserved for %s", key, pkg)
	}
	return key
}

func (c *checker) reportKey(keyExpr ast.Expr, msg, want string) {
	d := analysis.Diagnostic{Pos: keyExpr.Pos(), Message: msg}
	if lit, ok := ast.Unparen(keyExpr).(*ast.BasicLit); ok && lit.Kind == token.STRING {
		d.SuggestedFixes = []analysis.SuggestedFix{{
			Message:   "rename the key to " + want,
			TextEdits: []analysis.TextEdit{{Pos: lit.Pos(), End: lit.End(), NewText: []byte(strconv.Quote(want))}},
		}}
	}
	c.pass.Report(d)
}

func (c *checker) checkValue(keyExpr ast.Expr, key string, value ast.Expr) {
	t := c.pass.TypesInfo.TypeOf(value)
	if t == nil {
		return
	}
	if sel, ok := ast.Unparen(value).(*ast.SelectorExpr); ok && secretFields[sel.Sel.Name] {
		c.pass.Reportf(value.Pos(), "%s is a secret and is never logged", sel.Sel.Name)
		return
	}
	if isErrorType(t) && key != "error" && key != "errors" && !strings.HasSuffix(key, "_error") {
		c.pass.Reportf(keyExpr.Pos(), "key %q holds an error: use \"error\" (or a key ending in _error)", key)
	}
	if name, ok := c.objectType(t); ok {
		c.reportObject(keyExpr, value, name)
		return
	}
	// hclog.Fmt("%#v", obj): the object formatted as a whole.
	if call, ok := ast.Unparen(value).(*ast.CallExpr); ok && isNamed(t, hclogPath, "Format") {
		for _, a := range call.Args[min(1, len(call.Args)):] {
			if name, ok := c.objectType(c.pass.TypesInfo.TypeOf(a)); ok {
				ref := objectTypes[name]
				c.pass.Reportf(value.Pos(), "a %s is formatted as a whole: log its %s as %q", name, ref.field, ref.key)
			}
		}
	}
}

// objectType returns the name of the structs type that t is, or points to,
// if it is one of objectTypes.
func (c *checker) objectType(t types.Type) (string, bool) {
	if t == nil {
		return "", false
	}
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != structsPath {
		return "", false
	}
	_, ok = objectTypes[named.Obj().Name()]
	return named.Obj().Name(), ok
}

func (c *checker) reportObject(keyExpr, value ast.Expr, name string) {
	ref := objectTypes[name]
	d := analysis.Diagnostic{
		Pos:     value.Pos(),
		Message: fmt.Sprintf("a whole %s is logged: log its %s as %q", name, ref.field, ref.key),
	}
	if name == "NamespacedID" {
		d.Message = fmt.Sprintf("a whole %s is logged: log its ID as %q and its Namespace as \"namespace\"", name, ref.key)
	}
	lit, isLit := ast.Unparen(keyExpr).(*ast.BasicLit)
	base, simple := simpleExpr(value)
	if isLit && lit.Kind == token.STRING && simple {
		var newValue string
		switch {
		case name == "NamespacedID":
			// x.NamespacedID() or a NamespacedID value: its two fields.
			if call, ok := ast.Unparen(value).(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NamespacedID" && len(call.Args) == 0 {
					base = types.ExprString(sel.X)
				}
			}
			newValue = base + ".ID, \"namespace\", " + base + ".Namespace"
		default:
			newValue = base + "." + ref.field
		}
		d.SuggestedFixes = []analysis.SuggestedFix{{
			Message: "log the " + ref.field,
			TextEdits: []analysis.TextEdit{
				{Pos: lit.Pos(), End: lit.End(), NewText: []byte(strconv.Quote(ref.key))},
				{Pos: value.Pos(), End: value.End(), NewText: []byte(newValue)},
			},
		}}
	}
	c.pass.Report(d)
}

// simpleExpr returns the source of an identifier, a selector chain or a
// method call without arguments on one, which a fix may extend.
func simpleExpr(e ast.Expr) (string, bool) {
	e = ast.Unparen(e)
	ok := true
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident, *ast.SelectorExpr, nil:
		case *ast.CallExpr:
			if len(n.Args) > 0 {
				ok = false
			}
		default:
			ok = false
		}
		return ok
	})
	return types.ExprString(e), ok
}

func isErrorType(t types.Type) bool {
	return types.Identical(t, types.Universe.Lookup("error").Type())
}

func isNamed(t types.Type, path, name string) bool {
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == path && named.Obj().Name() == name
}

// snakeCase turns camelCase, dotted and dashed keys into lower snake_case.
func snakeCase(key string) string {
	var b strings.Builder
	runes := []rune(key)
	for i, r := range runes {
		switch {
		case r == '.' || r == '-' || r == ' ':
			b.WriteByte('_')
		case unicode.IsUpper(r):
			prevLower := i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]))
			nextLower := i > 0 && i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i+1])
			if prevLower || nextLower {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
