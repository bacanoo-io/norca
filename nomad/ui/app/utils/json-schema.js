/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// A small JSON Schema validator for the unified editor: the subset of the
// 2020-12 vocabulary the UI's schemas use (type, enum, const, required,
// properties, additionalProperties, patternProperties, items, minItems,
// maxItems, minLength, maxLength, pattern, minimum, maximum, oneOf, anyOf).
// Returns a list of { path, message } errors; an empty list means valid.

function typeOf(value) {
  if (value === null) return 'null';
  if (Array.isArray(value)) return 'array';
  if (Number.isInteger(value)) return 'integer';
  return typeof value;
}

function matchesType(value, type) {
  const actual = typeOf(value);
  if (type === 'number') return actual === 'number' || actual === 'integer';
  return actual === type;
}

function pointer(path) {
  return path.length ? path.join('.') : '(root)';
}

export function validate(schema, value, path = []) {
  if (!schema || schema === true) return [];
  if (schema === false)
    return [{ path: pointer(path), message: 'not allowed' }];
  const errors = [];
  const add = (message, at = path) =>
    errors.push({ path: pointer(at), message });

  if (schema.type) {
    const types = Array.isArray(schema.type) ? schema.type : [schema.type];
    if (!types.some((t) => matchesType(value, t))) {
      add(`must be ${types.join(' or ')}, not ${typeOf(value)}`);
      return errors;
    }
  }

  if (schema.enum && !schema.enum.some((e) => e === value)) {
    add(
      `must be one of ${schema.enum.map((e) => JSON.stringify(e)).join(', ')}`
    );
  }
  if ('const' in schema && schema.const !== value) {
    add(`must be ${JSON.stringify(schema.const)}`);
  }

  if (typeof value === 'string') {
    if (schema.minLength !== undefined && value.length < schema.minLength) {
      add(
        schema.minLength === 1
          ? 'must not be empty'
          : `must be at least ${schema.minLength} characters long`
      );
    }
    if (schema.maxLength !== undefined && value.length > schema.maxLength) {
      add(`must be at most ${schema.maxLength} characters long`);
    }
    if (schema.pattern && !new RegExp(schema.pattern, 'u').test(value)) {
      add(schema.patternMessage || `must match ${schema.pattern}`);
    }
  }

  if (typeof value === 'number') {
    if (schema.minimum !== undefined && value < schema.minimum) {
      add(`must be at least ${schema.minimum}`);
    }
    if (schema.maximum !== undefined && value > schema.maximum) {
      add(`must be at most ${schema.maximum}`);
    }
  }

  if (Array.isArray(value)) {
    if (schema.minItems !== undefined && value.length < schema.minItems) {
      add(`must have at least ${schema.minItems} items`);
    }
    if (schema.maxItems !== undefined && value.length > schema.maxItems) {
      add(`must have at most ${schema.maxItems} items`);
    }
    if (schema.items) {
      value.forEach((item, i) =>
        errors.push(...validate(schema.items, item, [...path, i]))
      );
    }
  }

  if (typeOf(value) === 'object') {
    (schema.required || []).forEach((key) => {
      if (!(key in value)) add(`${key} is required`);
    });
    const properties = schema.properties || {};
    const patterns = Object.entries(schema.patternProperties || {}).map(
      ([p, s]) => [new RegExp(p, 'u'), s]
    );
    Object.entries(value).forEach(([key, child]) => {
      let matched = false;
      if (key in properties) {
        matched = true;
        errors.push(...validate(properties[key], child, [...path, key]));
      }
      patterns.forEach(([re, s]) => {
        if (re.test(key)) {
          matched = true;
          errors.push(...validate(s, child, [...path, key]));
        }
      });
      if (!matched && schema.additionalProperties !== undefined) {
        if (schema.additionalProperties === false) {
          add(`unknown field ${key}`, [...path, key]);
        } else {
          errors.push(
            ...validate(schema.additionalProperties, child, [...path, key])
          );
        }
      }
    });
  }

  if (schema.anyOf) {
    const ok = schema.anyOf.some((s) => !validate(s, value, path).length);
    if (!ok) add(schema.anyOfMessage || 'does not match any allowed form');
  }
  if (schema.oneOf) {
    const count = schema.oneOf.filter(
      (s) => !validate(s, value, path).length
    ).length;
    if (count !== 1) add(schema.oneOfMessage || 'must match exactly one form');
  }

  return errors;
}

// Parses JSON and reports the position of a syntax error, for the editor.
export function parseJSON(text) {
  try {
    return { value: JSON.parse(text), error: null };
  } catch (e) {
    const message = String(e.message || e);
    const m = message.match(/position (\d+)/);
    let line = null;
    if (m) line = text.slice(0, Number(m[1])).split('\n').length;
    const lineMatch = message.match(/line (\d+)/);
    if (!line && lineMatch) line = Number(lineMatch[1]);
    return { value: undefined, error: { message, line } };
  }
}
