// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/resource"
)

// defaultName sets the first of Name, Path and ID that the spec left out
// to the name of the resource. A resource with a key or of a child module
// has no name of its own to give.
func defaultName(v reflect.Value, name string) error {
	for _, field := range []string{"Name", "Path", "ID"} {
		f := v.FieldByName(field)
		if !f.IsValid() || f.Kind() != reflect.String {
			continue
		}

		if f.String() != "" {
			return nil
		}

		if strings.ContainsAny(name, "[.") {
			return fmt.Errorf("a resource with for_each, count or of a child module writes its %s", strings.ToLower(field))
		}

		f.SetString(name)

		return nil
	}

	return nil
}

// releaseResource maps an evaluated resource onto the resource of the
// release API. A job is the API job the decoder made. Every other kind is
// the value of its block (design 4.5), mapped onto Norca's API type of the
// kind by the hcl tags of the type, else by its field names (job_acl:
// JobACL): the format Norca reads for the kind is its schema. A Name or
// Path left out is the name of the resource, a Namespace left out the
// namespace of the release.
func releaseResource(r resource.Resource, namespace string, secrets map[string]host.Sensitive) (*api.ReleaseResource, *api.ReleaseVariableSecret, error) {
	out := &api.ReleaseResource{Type: string(r.Kind)}

	payload, ok := payloads[r.Kind]
	if !ok {
		return nil, nil, fmt.Errorf("%s %s: the release API has no kind %s", r.Kind, r.Name, r.Kind)
	}

	target := payload(out)

	if err := decodeSpec(r, target); err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", r.Kind, r.Name, err)
	}

	if err := defaults(reflect.ValueOf(target).Elem(), r, namespace); err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", r.Kind, r.Name, err)
	}

	if r.Kind != resource.KindVariable || len(secrets) == 0 {
		return out, nil, nil
	}

	secret := &api.ReleaseVariableSecret{Namespace: out.Variable.Namespace, Path: out.Variable.Path, Items: map[string]string{}}
	for name, s := range secrets {
		out.SecretKeys = append(out.SecretKeys, name)
		secret.Items[name] = s.Reveal()
	}

	return out, secret, nil
}

// payloads set the payload of a resource of the release API for a kind,
// and return it to be decoded into.
var payloads = map[resource.Kind]func(*api.ReleaseResource) any{
	resource.KindJob:             func(r *api.ReleaseResource) any { r.Job = &api.Job{}; return r.Job },
	resource.KindNamespace:       func(r *api.ReleaseResource) any { r.Namespace = &api.Namespace{}; return r.Namespace },
	resource.KindQuota:           func(r *api.ReleaseResource) any { r.Quota = &api.QuotaSpec{}; return r.Quota },
	resource.KindACLPolicy:       func(r *api.ReleaseResource) any { r.ACLPolicy = &api.ACLPolicy{}; return r.ACLPolicy },
	resource.KindVariable:        func(r *api.ReleaseResource) any { r.Variable = &api.Variable{}; return r.Variable },
	resource.KindObjectKind:      func(r *api.ReleaseResource) any { r.ObjectKind = &api.ObjectKind{}; return r.ObjectKind },
	resource.KindAdmissionPolicy: func(r *api.ReleaseResource) any { r.AdmissionPolicy = &api.AdmissionPolicy{}; return r.AdmissionPolicy },
	resource.KindExternalService: func(r *api.ReleaseResource) any {
		r.ExternalService = &api.ExternalServiceItem{}
		return r.ExternalService
	},
	resource.KindObject: func(r *api.ReleaseResource) any { r.Object = &api.CustomObject{}; return r.Object },
}

// decodeSpec decodes the spec of a resource into the API type: a job as
// it is, another kind with its keys mapped onto the fields of the type.
func decodeSpec(r resource.Resource, target any) error {
	if r.Kind == resource.KindJob {
		return json.Unmarshal(r.Spec, target)
	}

	// A kind ResourceDecoder decodes is in the encoding of the API already;
	// an engine without the decoder gives the value of the block, mapped
	// below.
	if slices.Contains(ResourceDecoder{}.Kinds(), r.Kind) {
		dec := json.NewDecoder(bytes.NewReader(r.Spec))
		dec.DisallowUnknownFields()

		if err := dec.Decode(target); err == nil {
			return nil
		}
	}

	var value any
	if err := json.Unmarshal(r.Spec, &value); err != nil {
		return err
	}

	mapped, err := mapKeys(value, reflect.TypeOf(target))
	if err != nil {
		return err
	}

	data, err := json.Marshal(mapped)
	if err != nil {
		return err
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()

	return dec.Decode(target)
}

// mapKeys renames the keys of the objects of value to the JSON names of the
// fields of t that they stand for.
func mapKeys(value any, t reflect.Type) (any, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch v := value.(type) {
	case map[string]any:
		return mapObject(v, t)
	case []any:
		if t.Kind() == reflect.Interface {
			return v, nil
		}

		if t.Kind() != reflect.Slice {
			return nil, fmt.Errorf("a list where a %s is expected", t.Kind())
		}

		out := make([]any, len(v))

		for i, item := range v {
			mapped, err := mapKeys(item, t.Elem())
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}

			out[i] = mapped
		}

		return out, nil
	default:
		return value, nil
	}
}

// mapObject maps an object onto a struct, a map or an interface.
func mapObject(v map[string]any, t reflect.Type) (any, error) {
	if t.Kind() == reflect.Struct {
		return mapStruct(v, t)
	}

	if t.Kind() == reflect.Interface {
		return v, nil
	}

	if t.Kind() != reflect.Map {
		return nil, fmt.Errorf("an object where a %s is expected", t.Kind())
	}

	out := map[string]any{}

	for k, item := range v {
		mapped, err := mapKeys(item, t.Elem())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}

		out[k] = mapped
	}

	return out, nil
}

// mapStruct maps the keys of an object onto the fields of a struct. A
// block written once where the field is a list becomes a list of one.
func mapStruct(v map[string]any, t reflect.Type) (map[string]any, error) {
	out := map[string]any{}

	for k, item := range v {
		f, ok := fieldFor(t, k)
		if !ok {
			return nil, fmt.Errorf("unknown attribute %s", k)
		}

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}

		if obj, isObj := item.(map[string]any); isObj && ft.Kind() == reflect.Slice && ft.Elem().Kind() != reflect.Uint8 {
			item = []any{obj}
		}

		mapped, err := mapKeys(item, f.Type)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}

		out[jsonName(f)] = mapped
	}

	return out, nil
}

// fieldFor finds the field of t that the key k of a block stands for: by
// its hcl tag, by its JSON name, else by its name without the underscores
// of k.
func fieldFor(t reflect.Type, k string) (reflect.StructField, bool) {
	plain := strings.ReplaceAll(k, "_", "")

	var byName *reflect.StructField

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}

		if tag, _, _ := strings.Cut(f.Tag.Get("hcl"), ","); tag == k {
			return f, true
		}

		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == k {
			return f, true
		}

		if byName == nil && strings.EqualFold(f.Name, plain) {
			byName = &f
		}
	}

	if byName != nil {
		return *byName, true
	}

	return reflect.StructField{}, false
}

// jsonName is the key encoding/json reads a field by.
func jsonName(f reflect.StructField) string {
	if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag != "" && tag != "-" {
		return tag
	}

	return f.Name
}

// defaults sets the name of a resource and the namespace of the release
// where the spec leaves them out.
func defaults(v reflect.Value, r resource.Resource, namespace string) error {
	if r.Kind != resource.KindJob {
		if err := defaultName(v, r.Name); err != nil {
			return err
		}
	}

	if f := v.FieldByName("Namespace"); f.IsValid() && f.Kind() == reflect.String && f.String() == "" && r.Kind != resource.KindNamespace {
		f.SetString(namespace)
	}

	if r.Kind == resource.KindJob {
		if f := v.FieldByName("Namespace"); f.IsValid() && f.Kind() == reflect.Pointer && f.IsNil() {
			f.Set(reflect.ValueOf(&namespace))
		}
	}

	return nil
}
