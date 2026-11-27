/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';
import { normalize } from 'nomad-ui/utils/releases';

// Rigger, the package manager, on the servers (design/rig-integration.md
// 4.8): its registries are objects of the kind rigger.norca.io/Registry,
// synced when someone asks; a search reads the index of a registry at its
// last good sync, and the servers install a module of it as a release.

export const REGISTRY_GROUP = 'rigger.norca.io';
export const REGISTRY_KIND = 'Registry';

// The categories of a module or a stack (module.Categories of Rigger) and
// the icon of each, as the catalog shows them.
export const CATEGORIES = [
  'application',
  'database',
  'messaging',
  'observability',
  'platform',
  'networking',
  'security',
  'storage',
  'developer-tools',
  'ai',
  'other',
];

const CATEGORY_ICONS = {
  application: 'layout-grid',
  database: 'database',
  messaging: 'send',
  observability: 'chart-line',
  platform: 'stack-2',
  networking: 'network',
  security: 'shield-lock',
  storage: 'folder',
  'developer-tools': 'code',
  ai: 'cpu',
  other: 'box',
};

export function categoryIcon(category) {
  return CATEGORY_ICONS[category] || 'package';
}

export function categoryLabel(category) {
  if (!category) return '';
  const text = category.replace(/-/g, ' ');
  return text === 'ai' ? 'AI' : text[0].toUpperCase() + text.slice(1);
}

// An index synced longer ago than this is shown as old.
export const OLD_INDEX_DAYS = 7;

// The age of the last good sync of a registry, in days; null when it was
// never synced.
export function indexAgeDays(registry, now = Date.now()) {
  const at = Date.parse(registry?.status?.lastGoodSyncAt || '');
  if (Number.isNaN(at)) return null;
  return Math.floor((now - at) / 86400000);
}

const enc = encodeURIComponent;

// The registries: objects of the kind, with spec and status as written.
export async function listRiggerRegistries(token, region) {
  const r = await apiRequest(
    token,
    `/v1/objects/${REGISTRY_GROUP}/${REGISTRY_KIND}`,
    { params: { region } }
  );
  const list = Array.isArray(r.body) ? r.body : r.body?.Objects || [];
  return {
    ...r,
    body: list.map((o) => ({
      name: o.Name,
      modifyIndex: o.ModifyIndex,
      spec: o.Spec || {},
      status: o.Status || {},
    })),
  };
}

// Writes a registry; cas is the ModifyIndex the change is based on.
export function applyRiggerRegistry(token, region, name, spec, cas) {
  return apiRequest(
    token,
    `/v1/object/${REGISTRY_GROUP}/${REGISTRY_KIND}/${enc(name)}`,
    {
      method: 'PUT',
      params: { region, cas: cas === undefined ? undefined : String(cas) },
      data: {
        Group: REGISTRY_GROUP,
        Kind: REGISTRY_KIND,
        Name: name,
        Spec: spec,
      },
    }
  );
}

export function deleteRiggerRegistry(token, region, name) {
  return apiRequest(
    token,
    `/v1/object/${REGISTRY_GROUP}/${REGISTRY_KIND}/${enc(name)}`,
    { method: 'DELETE', params: { region } }
  );
}

export function syncRiggerRegistry(token, region, name) {
  return apiRequest(token, `/v1/rigger/registry/${enc(name)}/sync`, {
    method: 'PUT',
    params: { region },
  });
}

// query: { kind, text, category, keyword, offset, limit }; the answer is
// { Items, Total, Commit }, the items as the servers write them.
export function searchRiggerRegistry(token, region, name, query = {}) {
  const params = { region };
  for (const key of ['kind', 'text', 'category', 'keyword']) {
    if (query[key]) params[key] = query[key];
  }
  if (query.offset) params.offset = String(query.offset);
  if (query.limit) params.limit = String(query.limit);
  return apiRequest(token, `/v1/rigger/registry/${enc(name)}/search`, {
    params,
  });
}

// What an install form needs of a version of a module: Metadata, Readme,
// FormSchema, Versions, Meets, Unmet, Icon, Origin. The schema keeps its
// keys.
export async function riggerModuleDetail(
  token,
  region,
  registry,
  module,
  version
) {
  const r = await apiRequest(
    token,
    `/v1/rigger/registry/${enc(registry)}/module`,
    {
      params: { region, name: module, version: version || undefined },
    }
  );
  const body = r.body || {};
  return { ...r, body: { ...body, FormSchema: body.FormSchema || null } };
}

// The icon of a version of a module as an object URL, null when it has
// none; the caller revokes it.
export async function riggerModuleIcon(
  token,
  region,
  registry,
  module,
  version
) {
  const params = new URLSearchParams({ name: module });
  if (version) params.set('version', version);
  if (region) params.set('region', region);
  const response = await token.authorizedRawRequest(
    `/v1/rigger/registry/${enc(registry)}/icon?${params}`,
    { method: 'GET' }
  );
  if (!response.ok) return null;
  const blob = await response.blob();
  return URL.createObjectURL(blob);
}

// Installs (plan: plans) a release of a module of a registry. body:
// { Release, Registry, Module, Version, ValuesJSON, Adopt }.
export async function riggerInstall(token, region, namespace, body, plan) {
  const r = await apiRequest(token, '/v1/rigger/install', {
    method: 'PUT',
    params: { region, namespace, plan: plan ? 'true' : undefined },
    data: body,
  });
  return { ...r, body: normalize(r.body) };
}

function shellQuote(value) {
  return `'${String(value).replace(/'/g, `'\\''`)}'`;
}

// The same install through the HTTP API (the rigger program installs a
// module of a directory, not one of a registry by its name).
export function riggerInstallCommand({
  registry,
  module,
  version,
  release,
  namespace,
  values,
  adopt,
  form,
}) {
  const body = {
    Release: release || module,
    Registry: registry,
    Module: module,
  };
  if (version) body.Version = version;
  // A sensitive value is never shown.
  const secret = new Set(
    (form?.fields || []).filter((f) => f.secret).map((f) => f.key)
  );
  const shown = Object.fromEntries(
    Object.entries(values || {}).map(([k, v]) => [
      k,
      secret.has(k) ? '<secret>' : v,
    ])
  );
  if (Object.keys(shown).length) body.ValuesJSON = shown;
  if (adopt) body.Adopt = true;
  const ns = encodeURIComponent(namespace || 'default');
  return [
    'curl -X PUT -H "X-Nomad-Token: $NORCA_TOKEN" \\',
    `  "$NORCA_ADDR/v1/rigger/install?namespace=${ns}" \\`,
    `  -d ${shellQuote(JSON.stringify(body))}`,
  ].join('\n');
}

// The form schema of Rigger for the form of the UI: a variable with a
// secret block is read by the job from the secret store at run time, so
// the form shows where, and takes no value for it.
export function riggerFormSchema(schema) {
  if (!schema?.properties) return schema;
  const properties = {};
  for (const [key, p] of Object.entries(schema.properties)) {
    if (p['x-ui-widget'] === 'secret') continue;
    properties[key] = p;
  }
  return { ...schema, properties };
}

// The variables the job reads from the secret store, for the form to name.
export function riggerStoredSecrets(schema) {
  return Object.entries(schema?.properties || {})
    .filter(([, p]) => p['x-ui-widget'] === 'secret')
    .map(([key, p]) => ({
      key,
      label: p.title || key,
      path: p['x-ui-secret-path'],
      item: p['x-ui-secret-item'],
    }));
}

/**
 * The backend of the install form for a module of a Rigger registry: what
 * Packages::SchemaForm loads, plans and applies.
 */
export function riggerBackend({ token, registry, module }) {
  return {
    kind: 'rigger',
    noun: 'module',
    hasAdopt: true,
    async load(region, version) {
      const r = await riggerModuleDetail(
        token,
        region,
        registry,
        module,
        version
      );
      return { detail: r.body, version: r.body?.Metadata?.version || version };
    },
    schema(detail) {
      return riggerFormSchema(detail?.FormSchema);
    },
    storedSecrets(detail) {
      return riggerStoredSecrets(detail?.FormSchema);
    },
    versions(detail) {
      return detail?.Versions || [];
    },
    compatibility(detail) {
      if (!detail || detail.Meets !== false) return null;
      return (
        (detail.Unmet || []).join('; ') ||
        'The cluster does not meet its requirements.'
      );
    },
    body({ version, release, values, adopt }) {
      const body = {
        Release: release,
        Registry: registry,
        Module: module,
        Version: version,
      };
      if (Object.keys(values || {}).length) body.ValuesJSON = values;
      if (adopt) body.Adopt = true;
      return body;
    },
    async plan(region, namespace, release, body) {
      const r = await riggerInstall(token, region, namespace, body, true);
      return {
        changes: (r.body?.Changes || []).map((c) => ({
          Action: c.Action,
          Type: c.Kind,
          Name: c.Name,
        })),
        warnings: r.body?.Warnings || [],
      };
    },
    async apply(region, namespace, release, body) {
      const r = await riggerInstall(token, region, namespace, body, false);
      return {
        Revision: r.body?.Revision,
        Action: r.body?.Action,
        NextSteps: r.body?.NextSteps,
      };
    },
    command(state) {
      return riggerInstallCommand({ registry, module, ...state });
    },
  };
}

// A version of a published stack of a registry: Source (the address to
// apply it by), Metadata, Readme, FormSchema, Versions, Meets, Unmet,
// Warnings. The schema keeps its keys.
export async function riggerStackDetail(
  token,
  region,
  registry,
  stack,
  version
) {
  const r = await apiRequest(
    token,
    `/v1/rigger/registry/${enc(registry)}/stack`,
    {
      params: { region, name: stack, version: version || undefined },
    }
  );
  return r;
}

// A request of a stack: plan, status, apply or destroy. body: { Source,
// Version, ValuesJSON, Prune, Adopt }; an apply and a destroy answer the
// operation they started.
export function riggerStackRequest(token, region, namespace, action, body) {
  return apiRequest(token, `/v1/rigger/stack/${action}`, {
    method: 'PUT',
    params: { region, namespace },
    data: body,
  });
}

export const OPERATION_KIND = 'Operation';

// The operations of a namespace (objects of the kind
// rigger.norca.io/Operation), of one stack when stack is given, the newest
// first.
export async function listRiggerOperations(token, region, namespace, stack) {
  const r = await apiRequest(
    token,
    `/v1/objects/${REGISTRY_GROUP}/${OPERATION_KIND}`,
    {
      params: { region, namespace },
    }
  );
  const list = Array.isArray(r.body) ? r.body : r.body?.Objects || [];
  return list
    .map((o) => ({
      name: o.Name,
      namespace: o.Namespace,
      spec: o.Spec || {},
      status: o.Status || {},
    }))
    .filter((o) => !stack || o.spec.stack === stack)
    .sort((a, b) =>
      String(b.status.startedAt || '').localeCompare(
        String(a.status.startedAt || '')
      )
    );
}

// A page of the log of an operation after the sequence after, waiting up
// to wait for the first record: { Lines, Phase, Finished, LogUnavailable }.
export function riggerOperationLog(
  token,
  region,
  namespace,
  name,
  after,
  wait
) {
  return apiRequest(token, `/v1/rigger/operation/${enc(name)}/log`, {
    params: {
      region,
      namespace,
      after: after ? String(after) : undefined,
      wait: wait || undefined,
    },
  });
}

export function cancelRiggerOperation(token, region, namespace, name) {
  return apiRequest(token, `/v1/rigger/operation/${enc(name)}/cancel`, {
    method: 'PUT',
    params: { region, namespace },
  });
}

export function isFinishedOperation(phase) {
  return ['Succeeded', 'Failed', 'Cancelled', 'Interrupted'].includes(phase);
}

// The registry of the cluster and the name of a published stack by the
// address a stack is applied by: <namespace>/<name> of the global
// registry, or <repository>//stacks/<name> of a registry repository.
export function stackOfSource(registries, source) {
  const s = String(source || '');
  for (const reg of registries || []) {
    const repo = String(reg.status?.source || reg.spec?.source || '').replace(
      /\/+$/,
      ''
    );
    if (
      reg.spec?.type !== 'global' &&
      repo &&
      s.startsWith(`${repo}//stacks/`)
    ) {
      return { registry: reg.name, stack: s.slice(`${repo}//stacks/`.length) };
    }
  }
  const global = (registries || []).find((r) => r.spec?.type === 'global');
  if (global && /^[a-z0-9][a-z0-9_-]*\/[a-z0-9][a-z0-9_-]*$/.test(s)) {
    return { registry: global.name, stack: s };
  }
  return null;
}

/**
 * The backend of the form of a published stack of a registry: the form of
 * its variables, a plan of its releases, and an apply that starts an
 * operation (Packages::SchemaForm with @backend).
 */
export function riggerStackBackend({ token, registry, stack }) {
  let source = '';
  return {
    kind: 'rigger-stack',
    noun: 'stack',
    hasAdopt: true,
    hasReleaseName: false,
    applyLabel: 'Apply',
    async load(region, version) {
      const r = await riggerStackDetail(
        token,
        region,
        registry,
        stack,
        version
      );
      source = r.body?.Source || '';
      return { detail: r.body, version: r.body?.Metadata?.version || version };
    },
    schema(detail) {
      return riggerFormSchema(detail?.FormSchema);
    },
    storedSecrets(detail) {
      return riggerStoredSecrets(detail?.FormSchema);
    },
    versions(detail) {
      return detail?.Versions || [];
    },
    compatibility(detail) {
      if (!detail || detail.Meets !== false) return null;
      return (
        (detail.Unmet || []).join('; ') ||
        'The cluster does not meet its requirements.'
      );
    },
    body({ version, values, adopt, detail }) {
      const body = { Source: detail?.Source || source, Version: version };
      if (Object.keys(values || {}).length) body.ValuesJSON = values;
      if (adopt) body.Adopt = true;
      return body;
    },
    async plan(region, namespace, release, body) {
      const r = await riggerStackRequest(
        token,
        region,
        namespace,
        'plan',
        body
      );
      const plan = r.body?.Plan || {};
      const releases = plan.releases || [];
      return {
        changes: releases.map((p) => ({
          Action: p.action,
          Type: `release ${p.module?.source || ''} ${
            p.module?.version || ''
          }`.trim(),
          Namespace: p.release?.namespace,
          Name: p.release?.name,
        })),
        warnings: [
          ...(r.body?.Warnings || []),
          ...releases
            .filter((p) => p.takes_over)
            .map(
              (p) =>
                `${p.release?.namespace}/${p.release?.name} belongs to ${p.takes_over}: the apply takes it over only with adopt`
            ),
        ],
      };
    },
    async apply(region, namespace, release, body) {
      const r = await riggerStackRequest(
        token,
        region,
        namespace,
        'apply',
        body
      );
      return { Operation: r.body?.name, Stack: r.body?.spec?.stack };
    },
    command({ namespace, values, adopt, version, form }) {
      const secret = new Set(
        (form?.fields || []).filter((f) => f.secret).map((f) => f.key)
      );
      const shown = Object.fromEntries(
        Object.entries(values || {}).map(([k, v]) => [
          k,
          secret.has(k) ? '<secret>' : v,
        ])
      );
      const body = { Source: source, Version: version };
      if (Object.keys(shown).length) body.ValuesJSON = shown;
      if (adopt) body.Adopt = true;
      return [
        'curl -X PUT -H "X-Nomad-Token: $NORCA_TOKEN" \\',
        `  "$NORCA_ADDR/v1/rigger/stack/apply?namespace=${encodeURIComponent(
          namespace || 'default'
        )}" \\`,
        `  -d ${shellQuote(JSON.stringify(body))}`,
      ].join('\n');
    },
  };
}
