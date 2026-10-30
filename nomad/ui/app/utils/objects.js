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

// Writes an object (PUT); dryRun asks the server to validate it (schema,
// defaults, CEL rules, admission webhooks) and return it as it would be
// stored, without storing it. cas is the ModifyIndex the change is based on.
export function applyObject(
  token,
  region,
  { group, kind, name, namespace },
  object,
  { dryRun = false, cas } = {}
) {
  return apiRequest(
    token,
    `/v1/object/${enc(group)}/${enc(kind)}/${enc(name)}`,
    {
      method: 'PUT',
      params: {
        region,
        namespace,
        dry_run: dryRun ? 'true' : undefined,
        cas: cas === undefined ? undefined : String(cas),
      },
      data: object,
    }
  );
}

// Deletes an object with a deletion mode (background, foreground or
// orphan). With finalizers (or foreground) the object is only marked for
// deletion; the answer is the object as it is after the request.
export function deleteObject(
  token,
  region,
  { group, kind, name, namespace },
  { propagation, cas, dryRun = false } = {}
) {
  return apiRequest(
    token,
    `/v1/object/${enc(group)}/${enc(kind)}/${enc(name)}`,
    {
      method: 'DELETE',
      params: {
        region,
        namespace,
        propagation: propagation || undefined,
        dry_run: dryRun ? 'true' : undefined,
        cas: cas === undefined ? undefined : String(cas),
      },
    }
  );
}

// The scale subresource of an object: Replicas (desired, at the spec path
// of the kind) and StatusReplicas (observed, at the status path).
export function getScale(token, region, { group, kind, name, namespace }) {
  return apiRequest(
    token,
    `/v1/object/${enc(group)}/${enc(kind)}/${enc(name)}/scale`,
    { params: { region, namespace } }
  );
}

// Approves (or revokes) a type-bound webhook of a kind; management only.
export function approveWebhook(token, region, group, kind, webhook, revoke) {
  return apiRequest(
    token,
    `/v1/object-kind/${enc(group)}/${enc(kind)}/webhook/${enc(webhook)}/${
      revoke ? 'revoke' : 'approve'
    }`,
    { method: 'PUT', params: { region } }
  );
}

export function deleteKind(token, region, group, kind) {
  return apiRequest(token, `/v1/object-kind/${enc(group)}/${enc(kind)}`, {
    method: 'DELETE',
    params: { region },
  });
}

// The job meta of jobs owned by an object (the reserved owner key, see
// structs.ObjectOwnerMetaValue): "<namespace>/crd/<group with dashes>/<Kind>/<name>".
export const JOB_META_OWNER = 'norca.owner';
export const JOB_META_OWNER_KIND = 'norca.owner.kind';

export function ownerMetaValue({ group, kind, namespace, name }) {
  return `${namespace || ''}/crd/${String(group).replace(
    /\./g,
    '-'
  )}/${kind}/${name}`;
}

// The jobs an object owns, by the owner reference in their meta.
export function listOwnedJobs(token, region, ref) {
  return apiRequest(token, '/v1/jobs', {
    params: {
      region,
      namespace: '*',
      meta: 'true',
      filter: `Meta["${JOB_META_OWNER}"] == "${ownerMetaValue(ref)}"`,
    },
  });
}

// Whether an owner reference of an object points at the given object. An
// owner without a namespace is in the namespace of the owned object.
export function ownsObject(owner, child, parent) {
  if (!owner || owner.Kind !== `${parent.Group}/${parent.Kind}`) return false;
  if (owner.Name !== parent.Name) return false;
  if (owner.ID && parent.ID && owner.ID !== parent.ID) return false;
  const ns = owner.Namespace || child.Namespace || '';
  return ns === (parent.Namespace || '');
}

// The link of an owner reference of an object: another custom object, a
// release of the package manager, a job, or nothing (a Variable).
export function ownerLink(owner, object) {
  const namespace = owner.Namespace || object?.Namespace || '';
  if (owner.Kind === 'Job')
    return {
      label: `Job ${owner.Name}`,
      route: 'jobs.job.index',
      models: [`${owner.Name}@${namespace || 'default'}`],
    };
  if (owner.Kind === 'packs.norca.io/Release')
    return {
      label: `Release ${owner.Name}`,
      route: 'packages.release',
      models: [namespace || 'default', owner.Name],
    };
  const slash = owner.Kind.lastIndexOf('/');
  if (slash > 0)
    return {
      label: `${owner.Kind.slice(slash + 1)} ${owner.Name}`,
      route: 'objects.object',
      models: [
        owner.Kind.slice(0, slash),
        owner.Kind.slice(slash + 1),
        owner.Name,
      ],
      query: { namespace: namespace || null },
    };
  return { label: `${owner.Kind} ${owner.Name}` };
}

// The dry run of a kind write: the kind as it would be stored and the
// migration report of the stored objects.
export function planKind(token, region, group, kind, objectKind) {
  return apiRequest(token, `/v1/object-kind/${enc(group)}/${enc(kind)}/plan`, {
    method: 'POST',
    params: { region },
    data: objectKind,
  });
}

export function saveKind(token, region, group, kind, objectKind) {
  return apiRequest(token, `/v1/object-kind/${enc(group)}/${enc(kind)}`, {
    method: 'PUT',
    params: { region },
    data: objectKind,
  });
}

// The JSON Schema of the spec of a kind (the kind schema describes
// {spec, status}), or null.
export function specSchema(kind) {
  try {
    const schema = kind?.Schema ? JSON.parse(kind.Schema) : null;
    return schema?.properties?.spec || null;
  } catch (e) {
    return null;
  }
}

// The editable document of an object: what its owner writes.
export function objectDocument(object, kind) {
  const doc = {
    Name: object?.Name || '',
  };
  if (kind?.Namespaced) doc.Namespace = object?.Namespace || 'default';
  doc.Labels = object?.Labels || {};
  doc.Spec = object?.Spec || {};
  return doc;
}

// The schema of the editable document, with the spec schema of the kind.
export function objectDocumentSchema(kind) {
  const properties = {
    Name: {
      type: 'string',
      pattern: '^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,126}[a-zA-Z0-9])?$',
      patternMessage:
        'must be up to 128 letters, digits, ., _ and -, starting and ending with a letter or digit',
    },
    Labels: {
      type: ['object', 'null'],
      additionalProperties: { type: 'string' },
    },
    Spec: specSchema(kind) || { type: 'object' },
  };
  if (kind?.Namespaced) properties.Namespace = { type: 'string', minLength: 1 };
  return {
    type: 'object',
    required: ['Name', 'Spec'],
    additionalProperties: false,
    properties,
  };
}

const objectOrNull = { type: ['object', 'null'] };

// The editable document of an object kind: the kind with its Schema as
// JSON (the API carries it as a string) and without the Raft indexes.
export function kindDocument(kind) {
  if (!kind)
    return {
      Group: 'example.com',
      Kind: 'Database',
      Description: '',
      Namespaced: true,
      Version: 1,
      Schema: {
        type: 'object',
        properties: {
          spec: {
            type: 'object',
            required: ['size'],
            properties: {
              size: { type: 'integer', minimum: 1, default: 1 },
              engine: { type: 'string', enum: ['postgres', 'mysql'] },
            },
          },
          status: { type: 'object' },
        },
      },
      Rules: [],
      Columns: [{ Name: 'Size', Path: 'spec.size' }],
    };
  // eslint-disable-next-line no-unused-vars
  const { CreateIndex, ModifyIndex, Schema, ...rest } = kind;
  let schema = null;
  try {
    schema = Schema ? JSON.parse(Schema) : null;
  } catch (e) {
    schema = Schema;
  }
  const doc = { ...rest };
  if (schema !== null) doc.Schema = schema;
  return doc;
}

// The API body of an edited kind document.
export function kindBody(doc) {
  const body = { ...doc };
  if (body.Schema && typeof body.Schema !== 'string')
    body.Schema = JSON.stringify(body.Schema);
  return body;
}

export const KIND_SCHEMA = {
  type: 'object',
  required: ['Group', 'Kind'],
  additionalProperties: false,
  properties: {
    Group: {
      type: 'string',
      pattern:
        '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$',
      patternMessage: 'must be a lower case DNS subdomain',
    },
    Kind: {
      type: 'string',
      pattern: '^[A-Za-z][A-Za-z0-9]{0,62}$',
      patternMessage: 'must be a CamelCase name of letters and digits',
    },
    Description: { type: 'string' },
    Namespaced: { type: 'boolean' },
    Schema: { type: ['object', 'string', 'null'] },
    Rules: {
      type: ['array', 'null'],
      items: {
        type: 'object',
        required: ['Expression'],
        additionalProperties: false,
        properties: {
          Expression: { type: 'string', minLength: 1 },
          Message: { type: 'string' },
        },
      },
    },
    Columns: {
      type: ['array', 'null'],
      items: {
        type: 'object',
        required: ['Name', 'Path'],
        additionalProperties: false,
        properties: {
          Name: {
            type: 'string',
            pattern: '^[a-zA-Z0-9][a-zA-Z0-9 ._-]{0,63}$',
          },
          Path: { type: 'string', minLength: 1 },
        },
      },
    },
    Scale: objectOrNull,
    Replicated: { type: 'boolean', const: false },
    Version: { type: 'integer', minimum: 1 },
    Migration: objectOrNull,
    Webhooks: { type: ['array', 'null'], items: { type: 'object' } },
    Limits: objectOrNull,
  },
};

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
  if (object?.DeletionTime)
    return { tone: 'warning', label: 'deleting', behind };
  if (status(get('Degraded')) === 'True')
    return { tone: 'danger', label: 'degraded', behind };
  if (status(get('Ready')) === 'False')
    return { tone: 'warning', label: 'not ready', behind };
  if (behind) return { tone: 'info', label: 'reconciling', behind };
  if (status(get('Ready')) === 'True')
    return { tone: 'success', label: 'ready', behind };
  return { tone: 'neutral', label: 'unknown', behind };
}

// The deletion state of an object marked for deletion: what it waits for.
export function deletionState(object) {
  if (!object?.DeletionTime) return null;
  const finalizers = object.Finalizers || [];
  const mode = object.DeletionPropagation || 'background';
  let waiting;
  if (finalizers.length)
    waiting = `its controllers to remove the finalizers ${finalizers.join(
      ', '
    )}`;
  else if (mode === 'foreground')
    waiting = 'the objects and jobs it owns to be deleted first';
  else waiting = 'the garbage collector';
  return { mode, waiting, since: new Date(object.DeletionTime / 1e6) };
}

export const DELETION_MODES = [
  {
    value: 'background',
    label: 'Background',
    help: 'The object is deleted now; the objects and jobs it owns are deleted afterwards.',
  },
  {
    value: 'foreground',
    label: 'Foreground',
    help: 'The objects and jobs it owns are deleted first, then the object.',
  },
  {
    value: 'orphan',
    label: 'Orphan',
    help: 'Only the object is deleted; the objects and jobs it owns keep running.',
  },
];

export function prettySchema(schema) {
  if (!schema) return '';
  try {
    return JSON.stringify(JSON.parse(schema), null, 2);
  } catch (e) {
    return schema;
  }
}
