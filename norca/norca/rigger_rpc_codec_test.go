// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// riggerRPCTypes are the RPC types of Rigger that embed structs.WriteRequest
// or its kin: each one shadows the generated codec (rigger_rpc_codec.go).
func riggerRPCTypes() []any {
	return []any{
		&RiggerBundleImportRequest{}, &RiggerBundleListRequest{}, &RiggerBundleListResponse{},
		&RiggerBundleNameRequest{}, &RiggerBundleResponse{},
		&RiggerDetailRequest{}, &RiggerDetailResponse{}, &RiggerIconResponse{},
		&RiggerInstallRequest{}, &RiggerInstallResponse{},
		&RiggerOperationLogRequest{}, &RiggerOperationLogResponse{},
		&RiggerOperationNameRequest{}, &RiggerOperationResponse{},
		&RiggerRegistryNameRequest{}, &RiggerRegistryResponse{},
		&RiggerRegistrySearchRequest{}, &RiggerRegistrySearchResponse{},
		&RiggerStackDetailResponse{}, &RiggerStackPlanResponse{},
		&RiggerStackRequest{}, &RiggerStackStatusResponse{},
	}
}

// fillFields gives the plain fields of a struct a value that is not the
// zero one: strings, booleans, numbers, bytes and lists of strings, in the
// struct itself and in the structs it holds. The other fields stay empty.
func fillFields(v reflect.Value) {
	for i := range v.NumField() {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}

		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Uint64:
			f.SetUint(7)
		case reflect.Struct:
			fillFields(f)
		case reflect.Slice:
			switch f.Type().Elem().Kind() {
			case reflect.Uint8:
				f.SetBytes([]byte{1, 2, 3})
			case reflect.String:
				f.Set(reflect.ValueOf([]string{"a", "b"}))
			default:
			}
		default:
		}
	}
}

// The RPC types of Rigger keep their own fields on the wire. With -tags
// codegen_generated (the release builds) the embedded structs.WriteRequest
// and its kin have generated codecs, which an embedding type inherits: a
// server that was not the leader forwarded a stack request without its
// files, and the leader refused it ("a stack is either published (a source)
// or sent as files"); an answer came back with its index alone.
func TestRiggerRPC_MsgpackRoundTrip(t *testing.T) {
	ci.Parallel(t)

	for _, in := range riggerRPCTypes() {
		typ := reflect.TypeOf(in).Elem()

		t.Run(typ.Name(), func(t *testing.T) {
			fillFields(reflect.ValueOf(in).Elem())

			var buf bytes.Buffer

			must.NoError(t, codec.NewEncoder(&buf, structs.MsgpackHandle).Encode(in))

			out := reflect.New(typ).Interface()

			must.NoError(t, codec.NewDecoder(&buf, structs.MsgpackHandle).Decode(out))
			must.Eq(t, in, out)
		})
	}
}

// Every struct of this package that embeds a type of structs has codec
// methods of its own. Without them it would inherit the generated ones of
// the embedded type in the release builds and lose its own fields on the
// wire. The check reads the sources, so it holds in every build, and it
// finds a type that is added later.
func TestRPCTypes_ShadowTheGeneratedCodec(t *testing.T) {
	ci.Parallel(t)

	entries, err := os.ReadDir(".")
	must.NoError(t, err)

	embedding := map[string]bool{}
	methods := map[string]int{}
	fset := token.NewFileSet()

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
		must.NoError(t, err)

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && embedsStructsType(ts) {
						embedding[ts.Name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if name := codecReceiver(d); name != "" {
					methods[name]++
				}
			}
		}
	}

	var missing []string

	for name := range embedding {
		if methods[name] != 2 {
			missing = append(missing, name)
		}
	}

	slices.Sort(missing)
	must.SliceEmpty(t, missing, must.Sprint("types that embed a type of structs without CodecEncodeSelf and CodecDecodeSelf of their own"))

	// The round trip test names every such type of Rigger.
	var listed, found []string

	for _, v := range riggerRPCTypes() {
		listed = append(listed, reflect.TypeOf(v).Elem().Name())
	}

	for name := range embedding {
		if strings.HasPrefix(name, "Rigger") {
			found = append(found, name)
		}
	}

	slices.Sort(listed)
	slices.Sort(found)
	must.Eq(t, found, listed, must.Sprint("riggerRPCTypes lists the RPC types of Rigger"))
}

// embedsStructsType reports whether ts is a struct with an embedded field
// of a type of the package structs.
func embedsStructsType(ts *ast.TypeSpec) bool {
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		return false
	}

	for _, f := range st.Fields.List {
		if len(f.Names) != 0 {
			continue
		}

		expr := f.Type
		if star, ok := expr.(*ast.StarExpr); ok {
			expr = star.X
		}

		if sel, ok := expr.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "structs" {
				return true
			}
		}
	}

	return false
}

// codecReceiver returns the type a CodecEncodeSelf or CodecDecodeSelf
// method is declared on, or nothing for another function.
func codecReceiver(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 || (d.Name.Name != "CodecEncodeSelf" && d.Name.Name != "CodecDecodeSelf") {
		return ""
	}

	expr := d.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}

	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}

	return ""
}
