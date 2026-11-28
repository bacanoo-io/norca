// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package evalwire is what an engine and the child process it evaluates in
// send each other (rig-integration.md 4.6d): the files of a module or a
// stack, values with their marks, the results of an evaluation and its
// diagnostics, as length-prefixed JSON frames.
package evalwire

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	ctymsgpack "github.com/zclconf/go-cty/cty/msgpack"
)

// Value is a cty value on the wire: its type as JSON, the value as
// msgpack (which keeps unknown values), and the paths of its marks. The
// zero Value is cty.NilVal.
type Value struct {
	Type  json.RawMessage `json:"type,omitempty"`
	Data  []byte          `json:"data,omitempty"`
	Marks []PathMarks     `json:"marks,omitempty"`
}

// PathMarks are the marks of the value at a path.
type PathMarks struct {
	Path  []Step   `json:"path"`
	Marks []string `json:"marks"`
}

// Step is a step of a path: an attribute, or an index by a key.
type Step struct {
	Attr string `json:"attr,omitempty"`
	Key  *Value `json:"key,omitempty"`
}

// ErrMark is a mark that is no string: the evaluation marks with strings
// only (eval.Sensitive).
var ErrMark = errors.New("a mark that is no string")

// EncodeValue puts a value on the wire.
func EncodeValue(v cty.Value) (Value, error) {
	if v == cty.NilVal {
		return Value{}, nil
	}

	unmarked, pvms := v.UnmarkDeepWithPaths()

	ty := unmarked.Type()

	typ, err := ctyjson.MarshalType(ty)
	if err != nil {
		return Value{}, fmt.Errorf("the type of a value: %w", err)
	}

	data, err := ctymsgpack.Marshal(unmarked, ty)
	if err != nil {
		return Value{}, fmt.Errorf("a value: %w", err)
	}

	out := Value{Type: typ, Data: data}

	for _, pvm := range pvms {
		pm := PathMarks{Path: make([]Step, 0, len(pvm.Path))}

		for m := range pvm.Marks {
			s, ok := m.(string)
			if !ok {
				return Value{}, fmt.Errorf("%w: %T", ErrMark, m)
			}

			pm.Marks = append(pm.Marks, s)
		}

		for _, step := range pvm.Path {
			switch st := step.(type) {
			case cty.GetAttrStep:
				pm.Path = append(pm.Path, Step{Attr: st.Name})
			case cty.IndexStep:
				key, err := EncodeValue(st.Key)
				if err != nil {
					return Value{}, err
				}

				pm.Path = append(pm.Path, Step{Key: &key})
			default:
				return Value{}, fmt.Errorf("a path step %T", step)
			}
		}

		out.Marks = append(out.Marks, pm)
	}

	return out, nil
}

// Decode takes a value off the wire.
func (w Value) Decode() (cty.Value, error) {
	if len(w.Type) == 0 {
		return cty.NilVal, nil
	}

	ty, err := ctyjson.UnmarshalType(w.Type)
	if err != nil {
		return cty.NilVal, fmt.Errorf("the type of a value: %w", err)
	}

	v, err := ctymsgpack.Unmarshal(w.Data, ty)
	if err != nil {
		return cty.NilVal, fmt.Errorf("a value: %w", err)
	}

	if len(w.Marks) == 0 {
		return v, nil
	}

	pvms := make([]cty.PathValueMarks, 0, len(w.Marks))

	for _, pm := range w.Marks {
		path := make(cty.Path, 0, len(pm.Path))

		for _, st := range pm.Path {
			if st.Key == nil {
				path = append(path, cty.GetAttrStep{Name: st.Attr})

				continue
			}

			key, err := st.Key.Decode()
			if err != nil {
				return cty.NilVal, err
			}

			path = append(path, cty.IndexStep{Key: key})
		}

		marks := make(cty.ValueMarks, len(pm.Marks))
		for _, m := range pm.Marks {
			marks[m] = struct{}{}
		}

		pvms = append(pvms, cty.PathValueMarks{Path: path, Marks: marks})
	}

	return v.MarkWithPaths(pvms), nil
}

// Diagnostic is a diagnostic on the wire; the expression and the context
// of an hcl.Diagnostic stay behind.
type Diagnostic struct {
	Severity hcl.DiagnosticSeverity `json:"severity"`
	Summary  string                 `json:"summary"`
	Detail   string                 `json:"detail,omitempty"`
	Subject  *hcl.Range             `json:"subject,omitempty"`
	Context  *hcl.Range             `json:"context,omitempty"`
}

// EncodeDiagnostics puts diagnostics on the wire.
func EncodeDiagnostics(diags hcl.Diagnostics) []Diagnostic {
	if len(diags) == 0 {
		return nil
	}

	out := make([]Diagnostic, 0, len(diags))
	for _, d := range diags {
		out = append(out, Diagnostic{Severity: d.Severity, Summary: d.Summary, Detail: d.Detail, Subject: d.Subject, Context: d.Context})
	}

	return out
}

// DecodeDiagnostics takes diagnostics off the wire.
func DecodeDiagnostics(in []Diagnostic) hcl.Diagnostics {
	if len(in) == 0 {
		return nil
	}

	out := make(hcl.Diagnostics, 0, len(in))
	for _, d := range in {
		out = append(out, &hcl.Diagnostic{Severity: d.Severity, Summary: d.Summary, Detail: d.Detail, Subject: d.Subject, Context: d.Context})
	}

	return out
}
