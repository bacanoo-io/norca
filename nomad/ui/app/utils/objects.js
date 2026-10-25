/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';

// Object kinds (type definitions) and custom objects (K8s item 8). A kind
// defines the schema, CEL rules, list columns, the scale subresource and
// webhooks; an object has a user-owned spec and a controller-owned status
// with conditions, generation and observedGeneration.

const enc = encodeURIComponent;

export function kindId(kind) {
  return `${kind.Group}/${kind.Kind}`;
}

export function listKinds(token, region) {
  return apiRequest(token, '/v1/object-kinds', { params: { region } });
}

export function getKind(token, region, group, kind) {
  return apiRequest(token, `/v1/object-kind/${enc(group)}/${enc(kind)}`, {
    params: { region },
  });
}

export function listObjects(token, region, group, kind, namespace) {
  return apiRequest(token, `/v1/objects/${enc(group)}/${enc(kind)}`, {
    params: { region, namespace },
  });
}

export function getObject(token, region, { group, kind, name, namespace }) {
  return apiRequest(
    token,
    `/v1/object/${enc(group)}/${enc(kind)}/${enc(name)}`,
    { params: { region, namespace } }
  );
}

export function objectEvents(token, region, { group, kind, name, namespace }) {
  return apiRequest(
    token,
    `/v1/object/${enc(group)}/${enc(kind)}/${enc(name)}/events`,
    { params: { region, namespace } }
  );
}

// Evaluates a column path of a kind on an object: dotted keys from the
// object root (spec, status, metadata fields), [n] indexes and
// [?key=='value'] filters that pick the first matching list element, e.g.
// status.conditions[?type=='Ready'].status.
export function evaluatePath(object, path) {
  if (!path) return undefined;
  const root = {
    spec: object.Spec,
    status: object.Status,
    metadata: {
      name: object.Name,
      namespace: object.Namespace,
      labels: object.Labels,
    },
  };
  const tokens = String(path).match(/[^.[\]]+|\[[^\]]*\]/g) || [];
  let value = root;
  for (const token of tokens) {
    if (value === undefined || value === null) return undefined;
    if (token.startsWith('[')) {
      const inner = token.slice(1, -1).trim();
      const filter = inner.match(/^\?\s*([\w.-]+)\s*==\s*'([^']*)'$/);
      if (filter && Array.isArray(value)) {
        value = value.find((item) => String(item?.[filter[1]]) === filter[2]);
      } else if (/^\d+$/.test(inner) && Array.isArray(value)) {
        value = value[Number(inner)];
      } else {
        return undefined;
      }
    } else {
      value = value[token];
    }
  }
  return value;
}

export function display(value) {
  if (value === undefined || value === null || value === '') return '–';
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

export function conditions(object) {
  const list = object?.Status?.conditions || object?.Status?.Conditions || [];
  return Array.isArray(list) ? list : [];
}

// The health of an object from its conditions and generations: degraded,
// not ready, waiting for the controller (observedGeneration behind) or
// ready.
export function objectHealth(object) {
  const list = conditions(object);
  const get = (type) => list.find((c) => (c.type || c.Type) === type);
  const status = (c) => c && String(c.status ?? c.Status);
  const observed = object?.Status?.observedGeneration;
  const behind =
    observed !== undefined && observed !== null && observed < object.Generation;
  if (status(get('Degraded')) === 'True')
    return { tone: 'danger', label: 'degraded', behind };
  if (status(get('Ready')) === 'False')
    return { tone: 'warning', label: 'not ready', behind };
  if (behind) return { tone: 'info', label: 'reconciling', behind };
  if (status(get('Ready')) === 'True')
    return { tone: 'success', label: 'ready', behind };
  return { tone: 'neutral', label: 'unknown', behind };
}

export function prettySchema(schema) {
  if (!schema) return '';
  try {
    return JSON.stringify(JSON.parse(schema), null, 2);
  } catch (e) {
    return schema;
  }
}
