/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';

// The built-in secret store (secrets plan, "API, CLI and UI"). Listings
// and metadata never contain values; a value is read only by readSecret,
// which the leader audits. Callers keep values in component state only:
// never in the URL, the Ember Data store, events, notifications or logs.

// Secret paths are URL safe ([a-zA-Z0-9-_~/]); each segment is encoded
// anyway, the slashes stay.
function secretURL(path, suffix = '') {
  return `/v1/secret/${String(path)
    .split('/')
    .map(encodeURIComponent)
    .join('/')}${suffix}`;
}

export const SECRET_PATH_PATTERN = '^[a-zA-Z0-9\\-_~]+(/[a-zA-Z0-9\\-_~]+)*$';
export const SECRET_ITEM_KEY_PATTERN = '^[a-zA-Z0-9_.\\-]{1,128}$';

export function validSecretPath(path) {
  return (
    !!path && path.length <= 256 && new RegExp(SECRET_PATH_PATTERN).test(path)
  );
}

export function validItemKey(key) {
  return new RegExp(SECRET_ITEM_KEY_PATTERN).test(key || '');
}

export function listSecrets(token, region, namespace, prefix) {
  return apiRequest(token, '/v1/secrets', {
    params: {
      region,
      namespace: namespace || '*',
      prefix: prefix || undefined,
    },
  });
}

export function getSecretMetadata(token, region, namespace, path) {
  return apiRequest(token, secretURL(path), {
    params: { region, namespace, metadata: 'true' },
  });
}

// The value of a version (0: the current one). Audited by the leader.
export function readSecret(token, region, namespace, path, version) {
  return apiRequest(token, secretURL(path), {
    params: {
      region,
      namespace,
      version: version ? String(version) : undefined,
    },
  });
}

// Writes a new version (items) and/or the metadata (meta). cas makes the
// write conditional: the secret's ModifyIndex, or 0 for a new secret.
export function writeSecret(
  token,
  region,
  namespace,
  path,
  { items, meta, cas }
) {
  const data = {};
  if (items) data.Items = items;
  if (meta) data.Meta = meta;
  return apiRequest(token, secretURL(path), {
    method: 'PUT',
    params: {
      region,
      namespace,
      cas: cas === undefined || cas === null ? undefined : String(cas),
    },
    data,
  });
}

// Soft delete, undelete or destroy versions. Without versions, delete and
// undelete act on the current version and destroy removes the secret.
export function secretVersionsOp(
  token,
  region,
  namespace,
  path,
  op,
  { versions, cas } = {}
) {
  const method = op === 'delete' ? 'DELETE' : 'POST';
  const suffix = op === 'delete' ? '' : `/${op}`;
  return apiRequest(token, secretURL(path, suffix), {
    method,
    params: {
      region,
      namespace,
      versions: versions?.length ? versions.join(',') : undefined,
      cas: cas === undefined || cas === null ? undefined : String(cas),
    },
  });
}

export function sealStatus(token, region) {
  return apiRequest(token, '/v1/operator/seal', { params: { region } });
}

// The user metadata of a secret (the SecretMeta part of the API object).
export function secretMeta(secret) {
  return {
    Description: secret?.Description || '',
    Owner: secret?.Owner || '',
    Labels: secret?.Labels || {},
    ExpiresAt: secret?.ExpiresAt || null,
    RotateEvery: secret?.RotateEvery || 0,
    MaxVersions: secret?.MaxVersions || 10,
  };
}

export const SECRET_META_SCHEMA = {
  type: 'object',
  additionalProperties: false,
  properties: {
    Description: { type: 'string', maxLength: 1024 },
    Owner: { type: 'string', maxLength: 256 },
    Labels: {
      type: ['object', 'null'],
      additionalProperties: { type: 'string', maxLength: 512 },
    },
    ExpiresAt: {
      type: ['string', 'null'],
      pattern: '^\\d{4}-\\d{2}-\\d{2}T',
      patternMessage: 'must be an RFC 3339 time, e.g. 2027-01-31T00:00:00Z',
    },
    RotateEvery: { type: 'integer', minimum: 0 },
    MaxVersions: { type: 'integer', minimum: 1, maximum: 100 },
  },
};

export function shortHash(hash) {
  if (!hash) return '';
  const value = String(hash).replace(/^hmac-sha256:/, '');
  return value.length > 12 ? `${value.slice(0, 12)}…` : value;
}

// Durations of the API are nanoseconds.
export function formatInterval(ns) {
  if (!ns) return '';
  const hours = ns / 3.6e12;
  if (hours >= 24 && hours % 24 === 0) return `${hours / 24} days`;
  if (hours >= 1) return `${Math.round(hours * 10) / 10} hours`;
  return `${Math.round(ns / 6e10)} minutes`;
}

// Who accessed a secret, from the accessor of an audit record.
export function accessorLabel(accessor) {
  if (!accessor) return 'unknown';
  if (accessor.JobID) {
    const task = [accessor.JobID, accessor.TaskGroup, accessor.Task]
      .filter(Boolean)
      .join('/');
    return `task ${task}`;
  }
  if (accessor.AccessorID) return `token ${accessor.AccessorID.slice(0, 8)}`;
  if (accessor.NodeID) return `client ${accessor.NodeID.slice(0, 8)}`;
  return 'anonymous';
}

// The latest audit record per secret (namespace and path) of a batch of
// SecretAccess events, merged into `known` (a Map keyed "ns|path").
export function latestAccesses(events, known = new Map()) {
  const out = new Map(known);
  for (const e of events || []) {
    if (e.Topic !== 'SecretAccess') continue;
    const r = e.Payload?.SecretAccess || e.Payload?.Record || e.Payload;
    if (!r?.Path) continue;
    const key = `${r.Namespace || 'default'}|${r.Path}`;
    const time = r.Time ? new Date(r.Time).getTime() : 0;
    const prev = out.get(key);
    if (!prev || time >= prev.time) out.set(key, { ...r, time });
  }
  return out;
}

// --- Jobs that reference a secret path ---------------------------------

function globToRegExp(pattern) {
  return new RegExp(
    `^${String(pattern)
      .split('*')
      .map((p) => p.replace(/[.+?^${}()|[\]\\]/g, '\\$&'))
      .join('.*')}$`
  );
}

function expand(path, vars) {
  return String(path || '')
    .replace(/\$\{job\}/g, vars.job || '')
    .replace(/\$\{group\}/g, vars.group || '')
    .replace(/\$\{task\}/g, vars.task || '');
}

// nomadSecret "path" calls and the Vault-style {{ with secret "..." }} of
// templates, which the built-in store serves without a Vault.
const TEMPLATE_CALLS = /(?:nomadSecret|secret)\s+"([^"]+)"/g;

// How a job references the secret path: secret blocks (provider norca or
// unset), secret_access bindings (path patterns of the group or task) and
// template calls. Returns a list of { where, how } or an empty list.
export function jobSecretReferences(job, path) {
  const refs = [];
  const jobID = job.ID;
  for (const group of job.TaskGroups || []) {
    const gvars = { job: jobID, group: group.Name };
    for (const binding of group.SecretAccess || []) {
      if (globToRegExp(expand(binding.Path, gvars)).test(path))
        refs.push({
          where: `group ${group.Name}`,
          how: `secret_access ${binding.Path}`,
        });
    }
    for (const task of group.Tasks || []) {
      const vars = { ...gvars, task: task.Name };
      const where = `task ${group.Name}/${task.Name}`;
      for (const s of task.Secrets || []) {
        if (s.Provider && s.Provider !== 'norca') continue;
        if (expand(s.Path, vars) === path)
          refs.push({
            where,
            how: `secret "${s.Name}"${
              s.Version ? ` (version ${s.Version})` : ''
            }`,
          });
      }
      for (const binding of task.SecretAccess || []) {
        if (globToRegExp(expand(binding.Path, vars)).test(path))
          refs.push({ where, how: `secret_access ${binding.Path}` });
      }
      for (const t of task.Templates || []) {
        for (const m of String(t.EmbeddedTmpl || '').matchAll(TEMPLATE_CALLS)) {
          const called = expand(m[1], vars).replace(/^kv\/data\//, '');
          if (called === path)
            refs.push({ where, how: `template ${t.DestPath || ''}`.trim() });
        }
      }
    }
  }
  return refs;
}

// At most this many jobs of the namespace are read to find references.
export const JOB_SCAN_LIMIT = 50;

export async function findReferencingJobs(token, region, namespace, path) {
  const { body: stubs } = await apiRequest(token, '/v1/jobs', {
    params: { region, namespace },
  });
  const list = (stubs || []).filter((j) => !j.ParentID);
  const scanned = list.slice(0, JOB_SCAN_LIMIT);
  const jobs = await Promise.all(
    scanned.map((stub) =>
      apiRequest(token, `/v1/job/${encodeURIComponent(stub.ID)}`, {
        params: { region, namespace: stub.Namespace || namespace },
      })
        .then((r) => r.body)
        .catch(() => null)
    )
  );
  const references = [];
  for (const job of jobs.filter(Boolean)) {
    const refs = jobSecretReferences(job, path);
    if (refs.length)
      references.push({
        id: job.ID,
        name: job.Name || job.ID,
        namespace: job.Namespace || namespace,
        refs,
      });
  }
  return {
    references,
    scanned: scanned.length,
    total: list.length,
  };
}
