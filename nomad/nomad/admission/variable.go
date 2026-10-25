// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const (
	// CRDPathPrefix and CRDSchemaPathPrefix are the Variables path prefixes
	// of the CRD pattern: objects are stored at crd/<group>/<kind>/<name>,
	// their JSON Schema at crd-schema/<group>/<kind>.
	CRDPathPrefix       = "crd/"
	CRDSchemaPathPrefix = "crd-schema/"

	// SchemaItem is the item of a crd-schema Variable that holds the JSON
	// Schema document.
	SchemaItem = "schema"

	// SchemaEncodingItem is the optional item of a crd-schema Variable that
	// tells how the items of the objects are turned into the JSON instance
	// validated against the schema: "string" (default) validates the items
	// as an object of strings, "json" parses every item value as JSON first.
	SchemaEncodingItem = "encoding"

	SchemaEncodingString = "string"
	SchemaEncodingJSON   = "json"

	// maxCachedSchemas bounds the compiled schema cache.
	maxCachedSchemas = 512

	// schemaURL is the base URL compiled schemas are registered under. It is
	// never fetched.
	schemaURL = "https://schema.norca.invalid/crd-schema.json"
)

// VariableInput is a Variable as seen by the admission controllers.
type VariableInput struct {
	Namespace   string
	Path        string
	Items       map[string]string
	CreateIndex uint64
	ModifyIndex uint64
}

// Value returns the generic value CEL rules see as the "variable" or
// "oldVariable" variable. The operation ("set" or "delete") is included when
// it is not empty.
func (v *VariableInput) Value(operation string) map[string]any {
	if v == nil {
		return nil
	}
	items := make(map[string]any, len(v.Items))
	for k, val := range v.Items {
		items[k] = val
	}
	out := map[string]any{
		"namespace":    v.Namespace,
		"path":         v.Path,
		"items":        items,
		"create_index": v.CreateIndex,
		"modify_index": v.ModifyIndex,
	}
	if operation != "" {
		out["operation"] = operation
	}
	return out
}

// Review returns the wire form of the Variable for a webhook. The items are
// only included if includeItems is set.
func (v *VariableInput) Review(includeItems bool) *ReviewVariable {
	if v == nil {
		return nil
	}
	rv := &ReviewVariable{
		Namespace:   v.Namespace,
		Path:        v.Path,
		CreateIndex: v.CreateIndex,
		ModifyIndex: v.ModifyIndex,
	}
	if includeItems {
		rv.Items = maps.Clone(v.Items)
		if rv.Items == nil {
			rv.Items = map[string]string{}
		}
	}
	return rv
}

// CRDSchemaPath returns the path of the crd-schema Variable that holds the
// schema of the CRD object at path (crd/<group>/<kind>/<name>). It returns
// false if path is not the path of a CRD object.
func CRDSchemaPath(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, CRDPathPrefix)
	if !ok {
		return "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	return CRDSchemaPathPrefix + parts[0] + "/" + parts[1], true
}

// IsCRDSchemaPath returns true if path is the path of a crd-schema Variable
// (crd-schema/<group>/<kind>).
func IsCRDSchemaPath(path string) bool {
	rest, ok := strings.CutPrefix(path, CRDSchemaPathPrefix)
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// VariableSchema is the compiled JSON Schema of a crd-schema Variable.
type VariableSchema struct {
	schema   *jsonschema.Schema
	encoding string
}

// noLoader refuses to load any schema resource, so that a schema can only
// reference itself and the standard meta-schemas: never files of the server
// or remote URLs.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("loading %q is not allowed", url)
}

// CompileVariableSchema compiles the schema held by the items of a
// crd-schema Variable. Schemas without "$schema" are JSON Schema draft
// 2020-12.
func CompileVariableSchema(items map[string]string) (*VariableSchema, error) {
	raw, ok := items[SchemaItem]
	if !ok {
		return nil, fmt.Errorf("missing %q item", SchemaItem)
	}
	encoding := items[SchemaEncodingItem]
	switch encoding {
	case "":
		encoding = SchemaEncodingString
	case SchemaEncodingString, SchemaEncodingJSON:
	default:
		return nil, fmt.Errorf("invalid %q item %q, must be %q or %q",
			SchemaEncodingItem, encoding, SchemaEncodingString, SchemaEncodingJSON)
	}

	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid JSON in %q item: %v", SchemaItem, err)
	}

	c := jsonschema.NewCompiler()
	c.UseLoader(noLoader{})
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON Schema: %v", err)
	}
	return &VariableSchema{schema: sch, encoding: encoding}, nil
}

// Validate validates the items of a Variable against the schema.
func (s *VariableSchema) Validate(items map[string]string) error {
	instance := make(map[string]any, len(items))
	for k, v := range items {
		if s.encoding != SchemaEncodingJSON {
			instance[k] = v
			continue
		}
		parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(v))
		if err != nil {
			return fmt.Errorf("item %q is not valid JSON: %v", k, err)
		}
		instance[k] = parsed
	}

	err := s.schema.Validate(instance)
	if err == nil {
		return nil
	}
	var verr *jsonschema.ValidationError
	if errors.As(err, &verr) {
		return errors.New(formatValidationError(verr))
	}
	return err
}

// formatValidationError flattens the leaf causes of a validation error into
// a single line.
func formatValidationError(verr *jsonschema.ValidationError) string {
	printer := message.NewPrinter(language.English)
	var leaves []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			leaves = append(leaves, fmt.Sprintf("at %q: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(verr)
	return strings.Join(leaves, "; ")
}

type cachedSchema struct {
	modifyIndex uint64
	schema      *VariableSchema
	err         error
}

// SchemaCache caches compiled crd-schema Variables per namespace, path and
// modify index. It is safe for concurrent use.
type SchemaCache struct {
	lock  sync.Mutex
	cache map[string]*cachedSchema
}

// NewSchemaCache returns an empty schema cache.
func NewSchemaCache() *SchemaCache {
	return &SchemaCache{cache: make(map[string]*cachedSchema)}
}

// Schema returns the compiled schema of the crd-schema Variable, compiling it
// if it is not cached at the given modify index.
func (c *SchemaCache) Schema(namespace, path string, modifyIndex uint64, items map[string]string) (*VariableSchema, error) {
	key := namespace + "\x00" + path

	c.lock.Lock()
	cached, ok := c.cache[key]
	c.lock.Unlock()
	if ok && cached.modifyIndex == modifyIndex && modifyIndex != 0 {
		return cached.schema, cached.err
	}

	schema, err := CompileVariableSchema(items)
	c.lock.Lock()
	if len(c.cache) >= maxCachedSchemas {
		clear(c.cache)
	}
	c.cache[key] = &cachedSchema{modifyIndex: modifyIndex, schema: schema, err: err}
	c.lock.Unlock()
	return schema, err
}
