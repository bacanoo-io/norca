// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package objectschema compiles the JSON Schemas of custom object kinds,
// validates objects against them and fills in their default values.
//
// The schema describes the document {"spec": ..., "status": ...} of an
// object. Validation uses github.com/santhosh-tekuri/jsonschema (draft
// 2020-12 by default; the schema may declare an older draft with $schema).
// Schemas must be self-contained: references to other documents are never
// loaded, only references inside the schema ("#/$defs/...") work.
package objectschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	// maxDefaultDepth bounds the recursion of default filling, which
	// follows local references and could otherwise loop.
	maxDefaultDepth = 64

	// maxErrors is the maximum number of validation errors reported.
	maxErrors = 10
)

// Schema is a compiled object kind schema.
type Schema struct {
	compiled *jsonschema.Schema

	// root is the schema document as encoding/json decodes it, used to
	// find the default values.
	root map[string]any
}

// noLoader refuses to load any document, so a schema can't make the server
// read files or fetch URLs.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("loading %q is not allowed, object kind schemas must be self-contained", url)
}

// Compile compiles the JSON Schema document of the kind identified by
// group and kind.
func Compile(group, kind, doc string) (*Schema, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(doc), &root); err != nil {
		return nil, fmt.Errorf("schema is not a JSON object: %v", err)
	}
	if root == nil {
		return nil, errors.New("schema is not a JSON object")
	}

	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(doc)))
	if err != nil {
		return nil, fmt.Errorf("failed to parse schema: %v", err)
	}

	loc := fmt.Sprintf("https://objectkinds.norca.invalid/%s/%s.json", group, kind)
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	if err := c.AddResource(loc, parsed); err != nil {
		return nil, fmt.Errorf("invalid schema: %v", err)
	}
	compiled, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("invalid schema: %v", err)
	}

	s := &Schema{compiled: compiled, root: root}
	if err := s.checkDefaults(c, loc); err != nil {
		return nil, err
	}
	return s, nil
}

// checkDefaults validates every default value against the schema that
// declares it, so a kind can't fill in values that its own schema rejects.
func (s *Schema) checkDefaults(c *jsonschema.Compiler, loc string) error {
	var errs []string
	var walk func(schema map[string]any, ptr string, depth int)
	walk = func(schema map[string]any, ptr string, depth int) {
		if depth > maxDefaultDepth || len(errs) >= maxErrors {
			return
		}
		if def, ok := schema["default"]; ok {
			sub, err := c.Compile(loc + "#" + (&neturl.URL{Fragment: ptr}).EscapedFragment())
			if err != nil {
				errs = append(errs, fmt.Sprintf("default at %s: %v", ptrOrRoot(ptr), err))
			} else if err := describe(sub.Validate(copyJSON(def))); err != nil {
				errs = append(errs, fmt.Sprintf("default at %s: %v", ptrOrRoot(ptr), err))
			}
		}
		for _, key := range []string{"properties", "$defs", "definitions", "patternProperties"} {
			if m, ok := schema[key].(map[string]any); ok {
				for name, sub := range m {
					if sm, ok := sub.(map[string]any); ok {
						walk(sm, ptr+"/"+key+"/"+escapePointer(name), depth+1)
					}
				}
			}
		}
		for _, key := range []string{"items", "additionalProperties", "not"} {
			if sm, ok := schema[key].(map[string]any); ok {
				walk(sm, ptr+"/"+key, depth+1)
			}
		}
		for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
			if list, ok := schema[key].([]any); ok {
				for i, sub := range list {
					if sm, ok := sub.(map[string]any); ok {
						walk(sm, fmt.Sprintf("%s/%s/%d", ptr, key, i), depth+1)
					}
				}
			}
		}
	}
	walk(s.root, "", 0)
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid default values: %s", strings.Join(errs, "; "))
	}
	return nil
}

func ptrOrRoot(ptr string) string {
	if ptr == "" {
		return "/"
	}
	return ptr
}

func escapePointer(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

// Validate validates the object document {"spec": ..., "status": ...}.
func (s *Schema) Validate(doc map[string]any) error {
	return describe(s.compiled.Validate(any(doc)))
}

// describe turns a validation error into a one-line error listing the
// failing locations.
func describe(err error) error {
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return err
	}

	printer := message.NewPrinter(language.English)
	var msgs []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		loc := "/" + strings.Join(e.InstanceLocation, "/")
		msgs = append(msgs, fmt.Sprintf("at %s: %s", loc, e.ErrorKind.LocalizedString(printer)))
	}
	walk(verr)
	if len(msgs) == 0 {
		return errors.New(verr.Error())
	}
	sort.Strings(msgs)
	msgs = dedupe(msgs)
	if len(msgs) > maxErrors {
		msgs = append(msgs[:maxErrors], fmt.Sprintf("and %d more errors", len(msgs)-maxErrors))
	}
	return errors.New(strings.Join(msgs, "; "))
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// ApplyDefaults fills in the default values of the schema that are missing
// in doc, in place. A default is filled in for a missing property of an
// object that exists; defaults of nested properties are then filled into
// the inserted value too. Array items are defaulted with the items schema.
func (s *Schema) ApplyDefaults(doc map[string]any) {
	s.applyDefaults(s.root, doc, 0)
}

func (s *Schema) applyDefaults(schema map[string]any, value any, depth int) any {
	if schema == nil || depth > maxDefaultDepth {
		return value
	}

	if ref, ok := schema["$ref"].(string); ok {
		if target := s.resolve(ref); target != nil {
			value = s.applyDefaults(target, value, depth+1)
		}
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, sub := range all {
			if m, ok := sub.(map[string]any); ok {
				value = s.applyDefaults(m, value, depth+1)
			}
		}
	}

	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		// Sorted for deterministic results.
		names := make([]string, 0, len(props))
		for name := range props {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			sub, ok := props[name].(map[string]any)
			if !ok {
				continue
			}
			sub = s.deref(sub, depth)
			if _, present := v[name]; !present {
				def, hasDefault := sub["default"]
				if !hasDefault {
					continue
				}
				v[name] = copyJSON(def)
			}
			v[name] = s.applyDefaults(sub, v[name], depth+1)
		}
		if additional, ok := schema["additionalProperties"].(map[string]any); ok {
			for name, e := range v {
				if _, declared := props[name]; !declared {
					v[name] = s.applyDefaults(additional, e, depth+1)
				}
			}
		}
		return v
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, e := range v {
				v[i] = s.applyDefaults(items, e, depth+1)
			}
		}
		return v
	default:
		return value
	}
}

// deref follows the $ref chain of a schema to find a schema with a
// default value: {"$ref": "#/$defs/x"} has the default of its target
// unless it declares its own.
func (s *Schema) deref(schema map[string]any, depth int) map[string]any {
	for i := 0; i < maxDefaultDepth-depth; i++ {
		if _, ok := schema["default"]; ok {
			return schema
		}
		ref, ok := schema["$ref"].(string)
		if !ok {
			return schema
		}
		target := s.resolve(ref)
		if target == nil {
			return schema
		}
		if _, ok := target["default"]; ok {
			merged := make(map[string]any, len(schema)+1)
			for k, v := range schema {
				merged[k] = v
			}
			merged["default"] = target["default"]
			return merged
		}
		schema = target
	}
	return schema
}

// resolve returns the schema a local reference ("#/$defs/name") points to.
func (s *Schema) resolve(ref string) map[string]any {
	ptr, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil
	}
	var cur any = s.root
	if ptr == "" {
		return s.root
	}
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	m, _ := cur.(map[string]any)
	return m
}

// copyJSON returns a deep copy of a JSON value.
func copyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, e := range t {
			m[k] = copyJSON(e)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, e := range t {
			s[i] = copyJSON(e)
		}
		return s
	default:
		return v
	}
}
