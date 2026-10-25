// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// maxPatchOperations limits the size of a webhook patch.
const maxPatchOperations = 10000

// patchOperation is a single RFC 6902 JSON Patch operation.
type patchOperation struct {
	Op    string           `json:"op"`
	Path  string           `json:"path"`
	From  string           `json:"from,omitempty"`
	Value *json.RawMessage `json:"value,omitempty"`
}

// ApplyJSONPatch applies an RFC 6902 JSON Patch document to the JSON document
// doc and returns the patched document. Numbers are preserved exactly.
func ApplyJSONPatch(doc, patch []byte) ([]byte, error) {
	var ops []patchOperation
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, fmt.Errorf("invalid JSON patch: %w", err)
	}
	if len(ops) > maxPatchOperations {
		return nil, fmt.Errorf("JSON patch has more than %d operations", maxPatchOperations)
	}

	root, err := decodeJSON(doc)
	if err != nil {
		return nil, fmt.Errorf("invalid document: %w", err)
	}

	for i, op := range ops {
		root, err = applyOperation(root, op)
		if err != nil {
			return nil, fmt.Errorf("JSON patch operation %d (%s %s): %w", i, op.Op, op.Path, err)
		}
	}

	return json.Marshal(root)
}

func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected data after JSON value")
	}
	return v, nil
}

func applyOperation(root any, op patchOperation) (any, error) {
	value := func() (any, error) {
		if op.Value == nil {
			return nil, errors.New("missing value")
		}
		return decodeJSON(*op.Value)
	}

	switch op.Op {
	case "add":
		v, err := value()
		if err != nil {
			return nil, err
		}
		return addValue(root, op.Path, v)
	case "remove":
		root, _, err := removeValue(root, op.Path)
		return root, err
	case "replace":
		v, err := value()
		if err != nil {
			return nil, err
		}
		if op.Path == "" {
			return v, nil
		}
		root, _, err = removeValue(root, op.Path)
		if err != nil {
			return nil, err
		}
		return addValue(root, op.Path, v)
	case "move":
		if op.Path != op.From && strings.HasPrefix(op.Path, op.From+"/") {
			return nil, errors.New("cannot move a value into one of its children")
		}
		root, v, err := removeValue(root, op.From)
		if err != nil {
			return nil, err
		}
		return addValue(root, op.Path, v)
	case "copy":
		v, err := getValue(root, op.From)
		if err != nil {
			return nil, err
		}
		// Deep copy through a JSON round trip so later operations don't
		// modify both locations.
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if v, err = decodeJSON(b); err != nil {
			return nil, err
		}
		return addValue(root, op.Path, v)
	case "test":
		want, err := value()
		if err != nil {
			return nil, err
		}
		got, err := getValue(root, op.Path)
		if err != nil {
			return nil, err
		}
		if !jsonEqual(got, want) {
			return nil, errors.New("test failed")
		}
		return root, nil
	default:
		return nil, fmt.Errorf("unsupported operation %q", op.Op)
	}
}

// parsePointer splits an RFC 6901 JSON pointer into its unescaped tokens.
func parsePointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("invalid JSON pointer %q", pointer)
	}
	tokens := strings.Split(pointer[1:], "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

func arrayIndex(token string, length int, allowEnd bool) (int, error) {
	if allowEnd && token == "-" {
		return length, nil
	}
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, fmt.Errorf("invalid array index %q", token)
	}
	idx, err := strconv.Atoi(token)
	if err != nil || idx < 0 {
		return 0, fmt.Errorf("invalid array index %q", token)
	}
	max := length - 1
	if allowEnd {
		max = length
	}
	if idx > max {
		return 0, fmt.Errorf("array index %d out of bounds", idx)
	}
	return idx, nil
}

func getValue(root any, pointer string) (any, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	cur := root
	for _, t := range tokens {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[t]
			if !ok {
				return nil, fmt.Errorf("path %q not found", pointer)
			}
			cur = v
		case []any:
			idx, err := arrayIndex(t, len(c), false)
			if err != nil {
				return nil, err
			}
			cur = c[idx]
		default:
			return nil, fmt.Errorf("path %q not found", pointer)
		}
	}
	return cur, nil
}

// update replaces the container at the parent of pointer with the result of
// fn and returns the new root.
func update(root any, tokens []string, fn func(parent any, key string) (any, error)) (any, error) {
	if len(tokens) == 1 {
		return fn(root, tokens[0])
	}
	key := tokens[0]
	switch c := root.(type) {
	case map[string]any:
		child, ok := c[key]
		if !ok {
			return nil, fmt.Errorf("path segment %q not found", key)
		}
		nc, err := update(child, tokens[1:], fn)
		if err != nil {
			return nil, err
		}
		c[key] = nc
		return c, nil
	case []any:
		idx, err := arrayIndex(key, len(c), false)
		if err != nil {
			return nil, err
		}
		nc, err := update(c[idx], tokens[1:], fn)
		if err != nil {
			return nil, err
		}
		c[idx] = nc
		return c, nil
	default:
		return nil, fmt.Errorf("path segment %q not found", key)
	}
}

func addValue(root any, pointer string, value any) (any, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return value, nil
	}
	return update(root, tokens, func(parent any, key string) (any, error) {
		switch c := parent.(type) {
		case map[string]any:
			c[key] = value
			return c, nil
		case []any:
			idx, err := arrayIndex(key, len(c), true)
			if err != nil {
				return nil, err
			}
			c = append(c, nil)
			copy(c[idx+1:], c[idx:])
			c[idx] = value
			return c, nil
		default:
			return nil, fmt.Errorf("cannot add to a non-container value at %q", pointer)
		}
	})
}

func removeValue(root any, pointer string) (any, any, error) {
	tokens, err := parsePointer(pointer)
	if err != nil {
		return nil, nil, err
	}
	if len(tokens) == 0 {
		return nil, nil, errors.New("cannot remove the document root")
	}
	var removed any
	newRoot, err := update(root, tokens, func(parent any, key string) (any, error) {
		switch c := parent.(type) {
		case map[string]any:
			v, ok := c[key]
			if !ok {
				return nil, fmt.Errorf("path %q not found", pointer)
			}
			removed = v
			delete(c, key)
			return c, nil
		case []any:
			idx, err := arrayIndex(key, len(c), false)
			if err != nil {
				return nil, err
			}
			removed = c[idx]
			return append(c[:idx], c[idx+1:]...), nil
		default:
			return nil, fmt.Errorf("path %q not found", pointer)
		}
	})
	return newRoot, removed, err
}

// jsonEqual compares two decoded JSON values, treating numbers by value.
func jsonEqual(a, b any) bool {
	an, aok := a.(json.Number)
	bn, bok := b.(json.Number)
	if aok && bok {
		if an == bn {
			return true
		}
		af, aerr := an.Float64()
		bf, berr := bn.Float64()
		return aerr == nil && berr == nil && af == bf
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if w, ok := bv[k]; !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}
