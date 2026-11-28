// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package evalwire

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/internal/eval"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// EncodeInputs puts the values of variables on the wire.
func EncodeInputs(in map[string]eval.Input) (map[string]Input, error) {
	out := make(map[string]Input, len(in))

	for name, v := range in {
		w, err := EncodeValue(v.Value)
		if err != nil {
			return nil, fmt.Errorf("the value of %s: %w", name, err)
		}

		out[name] = Input{Value: w, Range: v.Range}
	}

	return out, nil
}

// DecodeInputs takes the values of variables off the wire.
func DecodeInputs(in map[string]Input) (map[string]eval.Input, error) {
	out := make(map[string]eval.Input, len(in))

	for name, w := range in {
		v, err := w.Value.Decode()
		if err != nil {
			return nil, fmt.Errorf("the value of %s: %w", name, err)
		}

		out[name] = eval.Input{Value: v, Range: w.Range}
	}

	return out, nil
}

// EncodeFacts and Decode carry the facts of an install.
func EncodeFacts(f eval.Facts) Facts { return Facts(f) }

// Decode takes the facts off the wire.
func (f Facts) Decode() eval.Facts { return eval.Facts(f) }

// EncodeResult puts an evaluated module on the wire.
func EncodeResult(r *eval.Result) (*Result, error) {
	if r == nil {
		return nil, nil
	}

	ov, err := EncodeValue(r.OutputValues)
	if err != nil {
		return nil, fmt.Errorf("the outputs: %w", err)
	}

	out := &Result{Resources: make([]Resource, 0, len(r.Resources)), OutputValues: ov}

	for _, res := range r.Resources {
		w := Resource{Kind: res.Kind, Name: res.Name, Spec: res.Spec, Hook: res.Hook}

		if len(res.Secrets) > 0 {
			w.Secrets = make(map[string]string, len(res.Secrets))
			for k, v := range res.Secrets {
				w.Secrets[k] = v.Reveal()
			}
		}

		out.Resources = append(out.Resources, w)
	}

	for _, o := range r.Outputs {
		out.Outputs = append(out.Outputs, Output(o))
	}

	return out, nil
}

// Decode takes an evaluated module off the wire. The fields only a test
// reads (the block, the key and the value of a resource) stay behind.
func (r *Result) Decode() (*eval.Result, error) {
	if r == nil {
		return nil, nil
	}

	ov, err := r.OutputValues.Decode()
	if err != nil {
		return nil, fmt.Errorf("the outputs: %w", err)
	}

	out := &eval.Result{Resources: make([]eval.Resource, 0, len(r.Resources)), OutputValues: ov}

	for _, w := range r.Resources {
		res := eval.Resource{Kind: w.Kind, Name: w.Name, Spec: w.Spec, Hook: w.Hook}

		if len(w.Secrets) > 0 {
			res.Secrets = make(map[string]host.Sensitive, len(w.Secrets))
			for k, v := range w.Secrets {
				res.Secrets[k] = host.NewSensitive(v)
			}
		}

		out.Resources = append(out.Resources, res)
	}

	for _, o := range r.Outputs {
		out.Outputs = append(out.Outputs, eval.Output(o))
	}

	return out, nil
}

// EncodeStackResult puts an evaluated stack on the wire, with the values
// it generated now.
func EncodeStackResult(res *eval.StackResult, generated map[string]string) (*StackResponse, error) {
	out := &StackResponse{Used: res.Generated, Missing: res.Missing, Generated: generated}

	for _, rr := range res.Releases {
		values, err := EncodeValue(rr.Values)
		if err != nil {
			return nil, fmt.Errorf("the values of the release %s: %w", rr.Name, err)
		}

		vars, err := EncodeValue(rr.Vars)
		if err != nil {
			return nil, fmt.Errorf("the variables of the release %s: %w", rr.Name, err)
		}

		result, err := EncodeResult(rr.Result)
		if err != nil {
			return nil, fmt.Errorf("the release %s: %w", rr.Name, err)
		}

		w := Release{
			Block: rr.Release.Name, Name: rr.Name, Enabled: rr.Enabled, Namespace: rr.Namespace,
			Values: values, Vars: vars, Result: result, DependsOn: rr.DependsOn, Pending: rr.Pending,
		}
		if rr.Name != rr.Release.Name {
			w.Part = strings.TrimSuffix(rr.Name, "-"+rr.Release.Name)
		}

		out.Releases = append(out.Releases, w)
	}

	for _, sec := range res.Secrets {
		out.Secrets = append(out.Secrets, Secret(sec))
	}

	for _, o := range res.Outputs {
		out.Outputs = append(out.Outputs, Output(o))
	}

	return out, nil
}

// Decode takes an evaluated stack off the wire; each release points at its
// block of s, the stack the engine loaded.
func (r *StackResponse) Decode(s *lang.Stack) (*eval.StackResult, error) {
	out := &eval.StackResult{Generated: r.Used, Missing: r.Missing}

	for _, w := range r.Releases {
		block, err := findRelease(s, w.Part, w.Block)
		if err != nil {
			return nil, err
		}

		values, err := w.Values.Decode()
		if err != nil {
			return nil, fmt.Errorf("the values of the release %s: %w", w.Name, err)
		}

		vars, err := w.Vars.Decode()
		if err != nil {
			return nil, fmt.Errorf("the variables of the release %s: %w", w.Name, err)
		}

		result, err := w.Result.Decode()
		if err != nil {
			return nil, fmt.Errorf("the release %s: %w", w.Name, err)
		}

		out.Releases = append(out.Releases, &eval.ReleaseResult{
			Release: block, Name: w.Name, Enabled: w.Enabled, Namespace: w.Namespace,
			Values: values, Vars: vars, Result: result, DependsOn: w.DependsOn, Pending: w.Pending,
		})
	}

	for _, sec := range r.Secrets {
		out.Secrets = append(out.Secrets, eval.StackSecret(sec))
	}

	for _, o := range r.Outputs {
		out.Outputs = append(out.Outputs, eval.Output(o))
	}

	return out, nil
}

// findRelease is the block of a release of s, or of one of its parts.
func findRelease(s *lang.Stack, part, name string) (*lang.StackRelease, error) {
	releases := s.Releases

	if part != "" {
		releases = nil

		for _, p := range s.Parts {
			if p.Name == part && p.Stack != nil {
				releases = p.Stack.Releases
			}
		}
	}

	for _, r := range releases {
		if r.Name == name {
			return r, nil
		}
	}

	if part != "" {
		name = part + "-" + name
	}

	return nil, fmt.Errorf("the child evaluated a release %s the stack does not have", name)
}
