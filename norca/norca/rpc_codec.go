// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"reflect"
	"sync"

	"github.com/hashicorp/go-msgpack/codec"
)

// encodeFields and decodeFields are the msgpack codec of the RPC types of
// this package that embed structs.WriteRequest and its kin. In the builds
// with the codegen_generated tag (the release builds) the embedded types
// have generated CodecEncodeSelf/CodecDecodeSelf methods, which an embedding
// type inherits: msgpack would encode the embedded part only. Such a type
// shadows the inherited methods with its own, which call these two: they
// encode every exported field, the embedded ones as fields of their own
// name.

// wireTypes caches the wire struct of a type: its exported fields, none of
// them embedded, so that the wire struct has no promoted methods.
var wireTypes sync.Map // reflect.Type -> wireType

type wireType struct {
	typ    reflect.Type
	fields []int // index of each wire field in the original struct
}

func wireTypeOf(t reflect.Type) wireType {
	if w, ok := wireTypes.Load(t); ok {
		return w.(wireType)
	}
	var w wireType
	var fields []reflect.StructField
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fields = append(fields, reflect.StructField{Name: f.Name, Type: f.Type, Tag: f.Tag})
		w.fields = append(w.fields, i)
	}
	w.typ = reflect.StructOf(fields)
	wireTypes.Store(t, w)
	return w
}

func encodeFields(e *codec.Encoder, v any) {
	src := reflect.ValueOf(v).Elem()
	w := wireTypeOf(src.Type())
	out := reflect.New(w.typ).Elem()
	for i, j := range w.fields {
		out.Field(i).Set(src.Field(j))
	}
	e.MustEncode(out.Addr().Interface())
}

func decodeFields(d *codec.Decoder, v any) {
	dst := reflect.ValueOf(v).Elem()
	w := wireTypeOf(dst.Type())
	in := reflect.New(w.typ)
	d.MustDecode(in.Interface())
	for i, j := range w.fields {
		dst.Field(j).Set(in.Elem().Field(i))
	}
}
