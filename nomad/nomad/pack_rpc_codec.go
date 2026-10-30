// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"reflect"
	"sync"

	"github.com/hashicorp/go-msgpack/codec"
)

// The RPC types of the pack registry embed structs.WriteRequest and its kin.
// In builds with the codegen_generated tag (the release builds) those have
// generated CodecEncodeSelf/CodecDecodeSelf methods, which an embedding type
// inherits: msgpack would encode the embedded part only, so a follower
// forwarded a Packs.Upsert to the leader without its Registry. These methods
// shadow the inherited ones and encode every field, the embedded ones as
// fields of their own name.

func (r *PackRegistryListRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *PackRegistryListRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *PackRegistryListResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *PackRegistryListResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }

func (r *PackRegistryUpsertRequest) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *PackRegistryUpsertRequest) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *PackRegistryNameRequest) CodecEncodeSelf(e *codec.Encoder)   { encodeFields(e, r) }
func (r *PackRegistryNameRequest) CodecDecodeSelf(d *codec.Decoder)   { decodeFields(d, r) }
func (r *PackRegistryResponse) CodecEncodeSelf(e *codec.Encoder)      { encodeFields(e, r) }
func (r *PackRegistryResponse) CodecDecodeSelf(d *codec.Decoder)      { decodeFields(d, r) }

func (r *PackDetailRequest) CodecEncodeSelf(e *codec.Encoder)     { encodeFields(e, r) }
func (r *PackDetailRequest) CodecDecodeSelf(d *codec.Decoder)     { decodeFields(d, r) }
func (r *PackDetailRPCResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *PackDetailRPCResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *PackDeployRPCRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *PackDeployRPCRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }

func (r *PackUploadRequest) CodecEncodeSelf(e *codec.Encoder)       { encodeFields(e, r) }
func (r *PackUploadRequest) CodecDecodeSelf(d *codec.Decoder)       { decodeFields(d, r) }
func (r *PackUploadDeleteRequest) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *PackUploadDeleteRequest) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }
func (r *PackUploadResponse) CodecEncodeSelf(e *codec.Encoder)      { encodeFields(e, r) }
func (r *PackUploadResponse) CodecDecodeSelf(d *codec.Decoder)      { decodeFields(d, r) }

func (r *PackPushNoticeRequest) CodecEncodeSelf(e *codec.Encoder)  { encodeFields(e, r) }
func (r *PackPushNoticeRequest) CodecDecodeSelf(d *codec.Decoder)  { decodeFields(d, r) }
func (r *PackPushNoticeResponse) CodecEncodeSelf(e *codec.Encoder) { encodeFields(e, r) }
func (r *PackPushNoticeResponse) CodecDecodeSelf(d *codec.Decoder) { decodeFields(d, r) }

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
