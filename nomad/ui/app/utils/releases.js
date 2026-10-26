/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';

// Pack releases (package-management-plan.md; pack/docs/releases.md). The
// typed views of a release carry Spec and Status whose keys are PascalCase
// in agents built with the generated codec and camelCase otherwise; the
// pages normalize them to PascalCase. User data (values, meta) keeps its
// keys.

const KEEP_KEYS = new Set(['Values', 'VariableValues', 'Meta', 'Labels']);

export function normalize(value) {
  if (Array.isArray(value)) return value.map(normalize);
  if (!value || typeof value !== 'object') return value;
  const out = {};
  for (const [key, v] of Object.entries(value)) {
    const k = key ? key[0].toUpperCase() + key.slice(1) : key;
    out[k] = KEEP_KEYS.has(k) ? v : normalize(v);
  }
  // acronyms of the Go names
  if ('Id' in out && !('ID' in out)) out.ID = out.Id;
  return out;
}

export const RELEASE_KIND = 'packs.norca.io/Release';

function path(name, suffix = '') {
  return `/v1/release/${encodeURIComponent(name)}${suffix}`;
}

export async function listReleases(token, region, namespace) {
  const r = await apiRequest(token, '/v1/releases', {
    params: { region, namespace },
  });
  return { ...r, body: normalize(r.body || []) };
}

export async function getRelease(token, region, namespace, name) {
  const r = await apiRequest(token, path(name), {
    params: { region, namespace },
  });
  return { ...r, body: normalize(r.body) };
}

export async function releaseHistory(token, region, namespace, name) {
  const r = await apiRequest(token, path(name, '/history'), {
    params: { region, namespace },
  });
  return { ...r, body: normalize(r.body || []) };
}

export async function releaseEvents(token, region, namespace, name) {
  return apiRequest(
    token,
    `/v1/object/${RELEASE_KIND}/${encodeURIComponent(name)}/events`,
    { params: { region, namespace } }
  );
}

export async function rollbackRelease(token, region, namespace, name, data) {
  const r = await apiRequest(token, path(name, '/rollback'), {
    method: 'POST',
    params: { region, namespace },
    data,
  });
  return { ...r, body: normalize(r.body) };
}

export async function testRelease(token, region, namespace, name) {
  const r = await apiRequest(token, path(name, '/test'), {
    method: 'POST',
    params: { region, namespace },
  });
  return { ...r, body: normalize(r.body) };
}

export async function checkDrift(token, region, namespace, name) {
  const r = await apiRequest(token, path(name, '/drift'), {
    method: 'POST',
    params: { region, namespace },
  });
  return { ...r, body: normalize(r.body) };
}

export function uninstallRelease(token, region, namespace, name, noHooks) {
  return apiRequest(token, path(name), {
    method: 'DELETE',
    params: {
      region,
      namespace,
      no_hooks: noHooks ? 'true' : undefined,
    },
  });
}

export function condition(release, type) {
  return (release?.Status?.Conditions || []).find((c) => c.Type === type);
}

export function isDrifted(release) {
  return condition(release, 'Drifted')?.Status === 'True';
}

// Server-verified signer, client-reported signer, or unsigned.
export function signature(release) {
  const pack = release?.Spec?.Pack || {};
  if (pack.TrustedSigner)
    return {
      tone: 'success',
      label: 'verified',
      title: `Verified by the server: ${pack.TrustedSigner}`,
    };
  if (pack.Signer)
    return {
      tone: 'info',
      label: 'signed',
      title: `Signature checked by the client: ${pack.Signer}`,
    };
  return {
    tone: 'warning',
    label: 'unsigned',
    title: 'The pack carries no signature',
  };
}

export function phaseTone(phase) {
  return (
    {
      deployed: 'success',
      superseded: 'neutral',
      failed: 'danger',
      pending: 'info',
      applying: 'info',
      'rolling-back': 'warning',
      uninstalling: 'warning',
    }[phase] || 'neutral'
  );
}

function flat(value, prefix = '', out = {}) {
  if (value && typeof value === 'object' && !Array.isArray(value)) {
    for (const [k, v] of Object.entries(value))
      flat(v, prefix ? `${prefix}.${k}` : k, out);
  } else {
    out[prefix] = value;
  }
  return out;
}

function text(v) {
  if (v === undefined) return '–';
  return typeof v === 'string' ? v : JSON.stringify(v);
}

// Comparison of two revisions: the values (flattened keys) and the
// resources (by type, namespace and name, changed when the hash differs).
export function compareRevisions(a, b) {
  const va = flat(a?.Spec?.Values || {});
  const vb = flat(b?.Spec?.Values || {});
  const values = [...new Set([...Object.keys(va), ...Object.keys(vb)])]
    .sort()
    .map((key) => ({
      key,
      a: text(va[key]),
      b: text(vb[key]),
      changed: JSON.stringify(va[key]) !== JSON.stringify(vb[key]),
    }));
  const id = (r) => `${r.Type}|${r.Namespace || ''}|${r.Name}`;
  const ra = new Map((a?.Spec?.Resources || []).map((r) => [id(r), r]));
  const rb = new Map((b?.Spec?.Resources || []).map((r) => [id(r), r]));
  const resources = [];
  for (const [k, r] of ra) {
    const other = rb.get(k);
    if (!other) resources.push({ ...r, change: 'removed' });
    else if (other.Hash !== r.Hash) resources.push({ ...r, change: 'changed' });
  }
  for (const [k, r] of rb)
    if (!ra.has(k)) resources.push({ ...r, change: 'added' });
  const pa = a?.Spec?.Pack || {};
  const pb = b?.Spec?.Pack || {};
  return {
    pack:
      pa.Version !== pb.Version
        ? `${pa.Name} ${pa.Version} → ${pb.Version}`
        : `${pa.Name} ${pa.Version} (same)`,
    values,
    changedValues: values.filter((v) => v.changed).length,
    resources,
  };
}

// The JSON Schema of the variables of the root pack of a revision
// (spec.variablesSchemas), or null.
export function valuesSchema(revision) {
  const packName = revision?.Spec?.Pack?.Name;
  const schemas = revision?.Spec?.VariablesSchemas || [];
  const root =
    schemas.find((s) => s.Pack === packName) ||
    schemas.find((s) => !String(s.Pack || '').includes('.')) ||
    null;
  if (!root?.Schema) return null;
  try {
    return JSON.parse(root.Schema);
  } catch (e) {
    return null;
  }
}

function shellQuote(value) {
  return `'${String(value).replace(/'/g, `'\\''`)}'`;
}

/**
 * The CLI command that upgrades a release with new variable values. Packs
 * are rendered by the client (pack/docs/releases.md), so the UI prepares
 * the command instead of applying the values itself. Values that are not
 * strings are passed as JSON (HCL accepts it for lists, maps and numbers).
 */
export function upgradeCommand(release, revision, values) {
  const pack = revision?.Spec?.Pack || release?.Spec?.Pack || {};
  const local = !pack.Registry || pack.Registry === '<<local folder>>';
  const source = local
    ? shellQuote(pack.Path || pack.Name || '<pack path>')
    : pack.Name;
  const parts = [`norca pack run ${source}`];
  if (!local) {
    parts.push(`--registry=${shellQuote(pack.Registry)}`);
    if (pack.Version) parts.push(`--ref=${shellQuote(pack.Version)}`);
  }
  parts.push(`--name=${shellQuote(release.Name)}`);
  if (release.Namespace && release.Namespace !== 'default')
    parts.push(`--namespace=${shellQuote(release.Namespace)}`);
  Object.keys(values || {})
    .sort()
    .forEach((key) => {
      const v = values[key];
      const text = typeof v === 'string' ? v : JSON.stringify(v);
      parts.push(`--var ${shellQuote(`${key}=${text}`)}`);
    });
  return parts.join(' \\\n  ');
}
