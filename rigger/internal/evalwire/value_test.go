// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package evalwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
)

const sensitive = "sensitive"

// A value crosses the wire with its type, its unknown parts and the marks
// of each path.
func TestValue_RoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value cty.Value
	}{
		{name: "nil", value: cty.NilVal},
		{name: "a string", value: cty.StringVal("x")},
		{name: "a marked string", value: cty.StringVal("pw").Mark(sensitive)},
		{name: "unknown", value: cty.UnknownVal(cty.String)},
		{name: "dynamic", value: cty.DynamicVal},
		{name: "null", value: cty.NullVal(cty.Number)},
		{
			name: "marks deep in an object, a map and a list",
			value: cty.ObjectVal(map[string]cty.Value{
				"size": cty.NumberIntVal(2),
				"db": cty.MapVal(map[string]cty.Value{
					"password": cty.StringVal("pw").Mark(sensitive),
					"user":     cty.StringVal("app"),
				}),
				"keys": cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b").Mark(sensitive)}),
				"all":  cty.TupleVal([]cty.Value{cty.True, cty.UnknownVal(cty.Number)}).Mark(sensitive),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			w, err := EncodeValue(tt.value)
			if err != nil {
				t.Fatal(err)
			}

			got, err := w.Decode()
			if err != nil {
				t.Fatal(err)
			}

			if tt.value == cty.NilVal {
				if got != cty.NilVal {
					t.Fatalf("nil came back as %#v", got)
				}

				return
			}

			if !got.RawEquals(tt.value) {
				t.Errorf("got %#v, want %#v", got, tt.value)
			}
		})
	}
}

// A mark that is no string can't cross the wire.
func TestValue_MarkNoString(t *testing.T) {
	t.Parallel()

	if _, err := EncodeValue(cty.StringVal("x").Mark(42)); !errors.Is(err, ErrMark) {
		t.Fatalf("err = %v, want ErrMark", err)
	}
}

// Diagnostics keep their severity, texts and ranges.
func TestDiagnostics_RoundTrip(t *testing.T) {
	t.Parallel()

	rng := &hcl.Range{Filename: "s/outputs.hcl", Start: hcl.Pos{Line: 2, Column: 3, Byte: 20}, End: hcl.Pos{Line: 2, Column: 9, Byte: 26}}
	in := hcl.Diagnostics{
		{Severity: hcl.DiagError, Summary: "Unsupported attribute", Detail: "No missing.", Subject: rng, Context: rng},
		{Severity: hcl.DiagWarning, Summary: "Deprecated"},
	}

	if got := DecodeDiagnostics(EncodeDiagnostics(in)); got.Error() != in.Error() || *got[0].Subject != *rng || got[1].Severity != hcl.DiagWarning {
		t.Fatalf("got %v, want %v", got, in)
	}

	if EncodeDiagnostics(nil) != nil || DecodeDiagnostics(nil) != nil {
		t.Fatal("no diagnostics did not stay none")
	}
}

// A frame carries one message; a frame past the bound is refused before it
// is read, and a cut one is an unexpected end.
func TestFrames(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteFrame(&buf, Request{Limits: Limits{Resources: 7}}); err != nil {
		t.Fatal(err)
	}

	var got Request
	if err := ReadFrame(&buf, &got); err != nil || got.Limits.Resources != 7 {
		t.Fatalf("read %+v, %v", got, err)
	}

	if err := ReadFrame(&buf, &got); !errors.Is(err, io.EOF) {
		t.Fatalf("an empty stream: %v, want io.EOF", err)
	}

	var head [4]byte
	binary.BigEndian.PutUint32(head[:], MaxFrame+1)

	if err := ReadFrame(bytes.NewReader(head[:]), &got); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("a frame past the bound: %v", err)
	}

	binary.BigEndian.PutUint32(head[:], 10)

	if err := ReadFrame(bytes.NewReader(append(head[:], 'x')), &got); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a cut frame: %v", err)
	}
}
