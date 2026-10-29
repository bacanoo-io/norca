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

const KEEP_KEYS = new Set([
  'Values',
  'VariableValues',
  'Meta',
  'Labels',
  'UIFormSchema',
]);

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

// Cluster pack registries (packs.norca.io/Registry objects) and the
// catalog of their packs; the servers sync and render them
// (nomad/nomad/pack_registry.go).
export async function listPackRegistries(token, region) {
  const r = await apiRequest(token, '/v1/pack-registries', {
    params: { region },
  });
  return { ...r, body: normalize(r.body || []) };
}

// spec: { type, source, defaultRef, trustedKeys }
export async function upsertPackRegistry(token, region, name, spec) {
  const r = await apiRequest(token, '/v1/pack-registry', {
    method: 'POST',
    params: { region },
    data: { Name: name, Spec: spec },
  });
  return { ...r, body: normalize(r.body) };
}

export async function syncPackRegistry(token, region, name) {
  const r = await apiRequest(
    token,
    `/v1/pack-registry/${encodeURIComponent(name)}/sync`,
    { method: 'POST', params: { region } }
  );
  return { ...r, body: normalize(r.body) };
}

export function deletePackRegistry(token, region, name) {
  return apiRequest(token, `/v1/pack-registry/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    params: { region },
  });
}

const REGISTRY_NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

// Sources compare without the scheme case, a trailing slash or ".git", and
// for OCI without the tag or digest of a pack location.
export function normalizeSource(source) {
  let s = String(source || '').trim();
  const i = s.indexOf('://');
  if (i > 0) s = s.slice(0, i).toLowerCase() + s.slice(i);
  if (s.startsWith('oci://')) {
    s = s.replace(/@sha256:[0-9a-f]+$/, '');
    const slash = s.lastIndexOf('/');
    const colon = s.lastIndexOf(':');
    if (colon > slash && slash > 'oci://'.length) s = s.slice(0, colon);
  }
  return s.replace(/\/+$/, '').replace(/\.git$/, '');
}

function sourceTag(source) {
  const s = String(source || '');
  if (!s.startsWith('oci://')) return '';
  const slash = s.lastIndexOf('/');
  const colon = s.lastIndexOf(':');
  return colon > slash ? s.slice(colon + 1) : '';
}

/**
 * The cluster registry to upgrade a release from: the registry the release
 * names, if it has the same source, else one with the source the CLI
 * recorded (pack.Source) that has the pack. Returns { registry, name,
 * version } or null.
 */
export function upgradeTarget(pack, registries) {
  if (!pack?.Name) return null;
  const regs = registries || [];
  const source = normalizeSource(pack.Source);
  const named = regs.find(
    (r) =>
      r.Name === pack.Registry &&
      (!source || normalizeSource(r.Spec?.Source) === source)
  );
  const bySource =
    source &&
    regs.find(
      (r) =>
        normalizeSource(r.Spec?.Source) === source &&
        (r.Status?.Packs || []).some((p) => p.Name === pack.Name)
    );
  const reg = named || bySource;
  return reg
    ? { registry: reg.Name, name: pack.Name, version: pack.Version || '' }
    : null;
}

/**
 * A cluster registry to add for a release installed with the CLI from a
 * git or OCI registry the cluster doesn't have: { name, type, source,
 * defaultRef }, or null (a local directory or a bundle).
 */
export function registrySuggestion(pack, registries) {
  const raw = String(pack?.Source || '').trim();
  if (!raw) return null;
  let type = null;
  if (raw.startsWith('oci://')) type = 'oci';
  else if (/^(https:\/\/|ssh:\/\/|[\w.-]+@[\w.-]+:)/.test(raw)) type = 'git';
  if (!type) return null;
  const taken = new Set((registries || []).map((r) => r.Name));
  let name = REGISTRY_NAME.test(pack.Registry || '') ? pack.Registry : '';
  if (!name || taken.has(name)) {
    const base =
      normalizeSource(raw)
        .split(/[/:]/)
        .filter(Boolean)
        .pop()
        ?.toLowerCase()
        .replace(/[^a-z0-9-]+/g, '-')
        .replace(/^-+|-+$/g, '') || 'registry';
    name = base;
    for (let n = 2; taken.has(name); n++) name = `${base}-${n}`;
  }
  const ref = type === 'oci' ? sourceTag(raw) || pack.Ref : pack.Ref;
  return {
    name,
    type,
    // git sources stay as recorded (some servers need the .git suffix).
    source: type === 'oci' ? normalizeSource(raw) : raw.replace(/\/+$/, ''),
    defaultRef: ref && ref !== 'latest' ? ref : '',
  };
}

export async function listCatalogPacks(token, region, registry) {
  const r = await apiRequest(token, '/v1/packs', {
    params: { region, registry: registry || undefined },
  });
  return { ...r, body: normalize(r.body || []) };
}

// { Pack, Versions, Readme, UIFormSchema }; the schema keeps its keys.
export async function getCatalogPack(token, region, registry, pack, version) {
  const r = await apiRequest(
    token,
    `/v1/pack/${encodeURIComponent(registry)}/${encodeURIComponent(pack)}`,
    { params: { region, version: version || undefined } }
  );
  return { ...r, body: normalize(r.body) };
}

// data: { Registry, Pack, Version, Values, Description }; the response is
// that of an apply (Release, Revision, Changes, Warnings).
export async function planPackRelease(token, region, namespace, name, data) {
  const r = await apiRequest(token, path(name, '/plan-pack'), {
    method: 'POST',
    params: { region, namespace },
    data,
  });
  return { ...r, body: normalize(r.body) };
}

export async function deployPackRelease(token, region, namespace, name, data) {
  const r = await apiRequest(token, path(name, '/deploy-pack'), {
    method: 'POST',
    params: { region, namespace },
    data,
  });
  return { ...r, body: normalize(r.body) };
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
