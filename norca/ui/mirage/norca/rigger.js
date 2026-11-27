/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Response } from 'miragejs';
import { collection, body, plain, error, json } from './util';
import { revision } from './releases';

// Mirage routes of Rigger on the servers (norca/command/agent/
// rigger_endpoint.go): the registries are objects of the kind
// rigger.norca.io/Registry (the object routes store them); MODULES is what
// the index of each registry holds, which a sync makes searchable.

export const GROUP = 'rigger.norca.io';
export const KIND = 'Registry';

// The schema of the kind, as the servers register it.
const KIND_SCHEMA = JSON.stringify({
  type: 'object',
  properties: {
    spec: {
      type: 'object',
      required: ['type', 'source'],
      properties: {
        type: { type: 'string', enum: ['global', 'git'] },
        source: { type: 'string', minLength: 1 },
        trustedKeys: { type: 'array', items: { type: 'string' } },
        authSecret: {
          type: 'object',
          required: ['path'],
          properties: {
            namespace: { type: 'string' },
            path: { type: 'string', minLength: 1 },
          },
        },
        display: {
          type: 'string',
          enum: ['catalog', 'selected'],
          default: 'catalog',
        },
        selected: { type: 'array', items: { type: 'object' } },
      },
    },
  },
});

// The form schema of the redis module: its password is a sensitive value,
// and the job reads a TLS key from the secret store.
const REDIS_SCHEMA = {
  $schema: 'https://json-schema.org/draft/2020-12/schema',
  type: 'object',
  title: 'redis',
  'x-ui-tabs': [
    { id: 'general', title: 'General', icon: 'sliders' },
    { id: 'resources', title: 'Resources & Scaling', icon: 'cpu' },
    { id: 'security', title: 'Security & Secrets', icon: 'lock' },
  ],
  properties: {
    job_name: {
      type: 'string',
      title: 'Job Name',
      default: 'redis',
      'x-ui-tab': 'general',
      'x-ui-group': 'Identity',
      'x-ui-order': 1,
    },
    mode: {
      type: 'string',
      title: 'Topology Mode',
      enum: ['standalone', 'replica', 'sentinel'],
      'x-ui-enum-labels': {
        standalone: 'Standalone',
        replica: 'Primary + Replicas',
        sentinel: 'High Availability (Sentinel)',
      },
      default: 'standalone',
      'x-ui-tab': 'general',
      'x-ui-group': 'Identity',
      'x-ui-widget': 'segmented',
      'x-ui-order': 2,
    },
    namespace: {
      type: 'string',
      title: 'Namespace',
      default: 'default',
      'x-ui-tab': 'general',
      'x-ui-group': 'Placement',
      'x-ui-widget': 'namespace-select',
      'x-ui-order': 3,
    },
    datacenters: {
      type: 'array',
      title: 'Datacenters',
      items: { type: 'string' },
      default: ['*'],
      'x-ui-tab': 'general',
      'x-ui-group': 'Placement',
      'x-ui-widget': 'datacenter-multiselect',
      'x-ui-order': 4,
    },
    replicas: {
      type: 'integer',
      title: 'Replicas',
      minimum: 1,
      maximum: 9,
      default: 2,
      'x-ui-tab': 'resources',
      'x-ui-group': 'Capacity & Scaling',
      'x-ui-widget': 'slider',
      'x-ui-unit': 'replicas',
      'x-ui-show-if': { field: 'mode', equals: 'replica' },
      'x-ui-order': 1,
    },
    resources: {
      type: 'object',
      title: 'Resources',
      properties: {
        cpu: {
          type: 'number',
          title: 'CPU',
          'x-ui-unit': 'MHz',
          'x-ui-presets': [250, 500, 1000],
          'x-ui-order': 10,
        },
        memory: {
          type: 'number',
          title: 'Memory',
          'x-ui-unit': 'MiB',
          'x-ui-presets': [256, 512, 1024],
          'x-ui-order': 20,
        },
      },
      default: { cpu: 500, memory: 256 },
      'x-ui-tab': 'resources',
      'x-ui-group': 'Capacity & Scaling',
      'x-ui-order': 2,
    },
    auth_enabled: {
      type: 'boolean',
      title: 'Require a password',
      default: false,
      'x-ui-tab': 'security',
      'x-ui-group': 'Authentication',
      'x-ui-widget': 'switch',
      'x-ui-order': 1,
    },
    redis_password: {
      type: 'string',
      title: 'Redis Password',
      'x-ui-tab': 'security',
      'x-ui-group': 'Authentication',
      'x-ui-secret': true,
      'x-ui-show-if': { field: 'auth_enabled', equals: true },
      'x-ui-order': 2,
    },
    extra_env: {
      type: 'object',
      title: 'Extra Env',
      additionalProperties: { type: 'string' },
      default: {},
      'x-ui-tab': 'general',
      'x-ui-group': 'Advanced',
      'x-ui-widget': 'key-value',
      'x-ui-order': 9,
    },
    tls_key: {
      type: 'string',
      title: 'TLS key',
      'x-ui-tab': 'security',
      'x-ui-widget': 'secret',
      'x-ui-secret-path': 'nomad/jobs/redis',
      'x-ui-secret-item': 'tls_key',
    },
  },
};

const APP_SCHEMA = {
  type: 'object',
  title: 'web-app',
  'x-ui-tabs': [
    { id: 'general', label: 'General', icon: 'settings', order: 10 },
  ],
  properties: {
    image: {
      type: 'string',
      title: 'Image',
      'x-ui-tab': 'general',
      'x-ui-group': 'General Configuration',
      'x-ui-widget': 'text',
      'x-ui-order': 10,
    },
  },
  required: ['image'],
};

function moduleOf(namespace, name, versions, extra = {}) {
  return { kind: 'module', namespace, name, versions, ...extra };
}

// The index of each registry source: its items, and for each the detail of
// its versions.
export const MODULES = {
  'github.com/bacanoo-io/norca-global-registry': [
    moduleOf('norca', 'redis', ['0.3.0', '0.2.0'], {
      description: 'In-memory data store, standalone or with replicas',
      category: 'database',
      keywords: ['cache', 'kv'],
      license: 'BSD-3-Clause',
      schema: REDIS_SCHEMA,
      readme: '# redis\n\nIn-memory data store.\n',
      icon: true,
    }),
    moduleOf('norca', 'nats', ['1.1.0'], {
      description: 'A NATS cluster per region',
      category: 'messaging',
      keywords: ['queue'],
      license: 'Apache-2.0',
      schema: { type: 'object', properties: {} },
      readme: '',
      unmet: ['Norca >= 9.0.0'],
    }),
    moduleOf('acme', 'shop', ['2.0.0'], {
      description: 'A web shop',
      category: 'application',
      keywords: ['shop'],
      license: 'MPL-2.0',
      schema: { type: 'object', properties: {} },
    }),
  ],
  'https://git.example.com/acme/rigger-modules': [
    moduleOf('', 'web-app', ['1.1.0', '1.0.0'], {
      description: 'A web application',
      category: 'application',
      schema: APP_SCHEMA,
    }),
    {
      kind: 'stack',
      namespace: '',
      name: 'site',
      versions: ['1.1.0', '1.0.0'],
      description: 'A web site with its database',
      category: 'platform',
      keywords: ['web'],
      readme: '# site\n\nA web application and its database.\n',
      schema: {
        type: 'object',
        properties: {
          domain: { type: 'string', title: 'Domain', 'x-ui-order': 10 },
          replicas: {
            type: 'number',
            title: 'Replicas',
            default: 2,
            'x-ui-order': 20,
          },
        },
        required: ['domain'],
      },
      // The releases of an apply: their names and modules.
      releases: [
        { name: 'site-db', module: 'postgres', part: 'data' },
        { name: 'site-web', module: 'web-app', part: 'web' },
      ],
    },
  ],
};

export const OPERATION_KIND = 'Operation';

// The address a stack request names a published stack by.
function stackSource(reg, item) {
  return reg.Spec.type === 'global'
    ? `${item.namespace}/${item.name}`
    : `${reg.Spec.source}//stacks/${item.name}`;
}

const DAY = 86400000;

function registryObject(name, spec, status, index) {
  return {
    Group: GROUP,
    Kind: KIND,
    Name: name,
    Namespace: '',
    Labels: {},
    Spec: spec,
    Status: status,
    Generation: 1,
    CreateIndex: index,
    ModifyIndex: index,
    Events: [],
  };
}

function strip(item) {
  // eslint-disable-next-line no-unused-vars
  const { schema, readme, icon, unmet, ...rest } = item;
  return rest;
}

// What a good sync of a source finds, for the status of a registry.
function syncedStatus(spec, at) {
  const items = MODULES[spec.source] || [];
  const ref = (i) => `${i.kind}/${i.namespace || ''}/${i.name}`;
  const selected = (spec.selected || []).map(
    (s) => `${s.kind}/${s.namespace || ''}/${s.name}`
  );
  return {
    phase: 'Ready',
    lastSyncedAt: at,
    lastGoodSyncAt: at,
    source: spec.source,
    commit: '0123456789abcdef0123456789abcdef01234567',
    modules: items.filter((i) => i.kind === 'module').length,
    stacks: items.filter((i) => i.kind === 'stack').length,
    ...(spec.display === 'selected'
      ? {
          shown: items.filter((i) => selected.includes(ref(i))).map(strip),
          missing: (spec.selected || []).filter(
            (s) =>
              !items.some(
                (i) => ref(i) === `${s.kind}/${s.namespace || ''}/${s.name}`
              )
          ),
        }
      : {}),
  };
}

export function seedRigger(server) {
  const db = server.db;
  collection(db, 'norcaObjectKinds').insert({
    Group: GROUP,
    Kind: KIND,
    Description:
      'Registry of modules and stacks of Rigger, the package manager, synced when asked for (built in)',
    Namespaced: false,
    Schema: KIND_SCHEMA,
    Columns: [{ Name: 'Type', Path: 'spec.type' }],
  });
  const objects = collection(db, 'norcaObjects');
  const global = {
    type: 'global',
    source: 'github.com/bacanoo-io/norca-global-registry',
    display: 'selected',
    selected: [{ kind: 'module', namespace: 'norca', name: 'redis' }],
  };
  objects.insert(
    registryObject(
      'global',
      global,
      syncedStatus(global, new Date(Date.now() - 2 * DAY).toISOString()),
      10
    )
  );
  const acme = {
    type: 'git',
    source: 'https://git.example.com/acme/rigger-modules',
    display: 'catalog',
  };
  objects.insert(
    registryObject(
      'acme',
      acme,
      syncedStatus(acme, new Date(Date.now() - 10 * DAY).toISOString()),
      11
    )
  );

  collection(db, 'norcaObjectKinds').insert({
    Group: GROUP,
    Kind: OPERATION_KIND,
    Description: 'An apply or a destroy of a stack of Rigger (built in)',
    Namespaced: true,
    Columns: [{ Name: 'Phase', Path: 'status.phase' }],
  });
}

// A stack applied before (after seedRigger): the site of acme in the
// default namespace, its releases and the operation that applied it.
export function seedRiggerStack(server) {
  const db = server.db;
  const repo = 'https://git.example.com/acme/rigger-modules';
  const site = MODULES[repo].find((i) => i.kind === 'stack');
  const source = `${repo}//stacks/site`;
  for (const r of site.releases) {
    insertStackRelease(db, 'default', 'site', source, '1.0.0', r);
  }
  insertOperation(db, 'default', 'op-site-1', {
    stack: 'site',
    action: 'apply',
    source,
    version: '1.0.0',
    phase: 'Succeeded',
    startedAt: new Date(Date.now() - DAY).toISOString(),
    releases: site.releases,
  });
}

// A release a stack owns, as the servers record it.
function insertStackRelease(db, namespace, stack, source, version, r) {
  const c = collection(db, 'norcaReleases');
  const existing = c.where(
    (x) => x.Name === r.name && (x.Namespace || 'default') === namespace
  )[0];
  const n = (existing?.Spec.revision || 0) + 1;
  const pack = {
    name: r.module,
    version: '1.0.0',
    manager: 'rigger',
    source: r.module,
  };
  const record = {
    Region: 'global',
    ID: existing?.ID || `rel-${r.name}`,
    Namespace: namespace,
    Name: r.name,
    Spec: {
      pack,
      revision: n,
      maxHistory: 10,
      owner: {
        kind: 'stack',
        namespace,
        name: stack,
        version,
        source,
        part: r.part,
      },
    },
    Status: {
      phase: 'deployed',
      revision: n,
      resources: [{ type: 'job', namespace, name: r.name, hook: '' }],
      conditions: [],
    },
    History: [
      revision(namespace, r.name, n, '1.0.0', {
        spec: { pack, values: {} },
        status: { phase: 'deployed', hooks: [] },
      }),
      ...(existing?.History || []),
    ],
    Events: [],
    Drift: [],
  };
  if (existing) c.update(existing.id, record);
  else c.insert(record);
  return n;
}

// An operation of a stack, finished: its outcomes and its log.
function insertOperation(db, namespace, name, o) {
  const at = o.startedAt || new Date().toISOString();
  const outcomes = o.releases.map((r) => ({
    namespace,
    release: r.name,
    action: o.action === 'destroy' ? 'uninstall' : 'install',
    revision: r.revision,
  }));
  const log = [
    { time: at, level: 'INFO', msg: `stack.${o.action}`, stack: o.stack },
    ...outcomes.map((x) => ({
      time: at,
      level: 'INFO',
      msg: `release.${x.action}`,
      release: `${namespace}/${x.release}`,
    })),
    { time: at, level: 'INFO', msg: `stack.${o.action} done`, stack: o.stack },
  ];
  const obj = {
    Group: GROUP,
    Kind: OPERATION_KIND,
    Name: name,
    Namespace: namespace,
    Labels: {},
    Spec: {
      stack: o.stack,
      action: o.action,
      source: o.source,
      version: o.version,
      requestedBy: 'management',
    },
    Status: {
      phase: o.phase,
      plan: outcomes,
      outcomes,
      startedAt: at,
      finishedAt: at,
      logStored: true,
    },
    Generation: 1,
    CreateIndex: 20,
    ModifyIndex: 20,
    Events: [],
    Log: log,
  };
  collection(db, 'norcaObjects').insert(obj);
  return obj;
}

function operationOf(db, name, namespace) {
  return collection(db, 'norcaObjects').where(
    (o) =>
      o.Group === GROUP &&
      o.Kind === OPERATION_KIND &&
      o.Name === name &&
      (o.Namespace || 'default') === (namespace || 'default')
  )[0];
}

function operationView(o) {
  return {
    namespace: o.Namespace,
    name: o.Name,
    spec: o.Spec,
    status: o.Status,
  };
}

// The published stack a stack request names: its registry and item.
function publishedStack(db, source) {
  for (const reg of collection(db, 'norcaObjects').where(
    (o) => o.Group === GROUP && o.Kind === KIND
  )) {
    const item = (MODULES[reg.Spec.source] || []).find(
      (i) => i.kind === 'stack' && stackSource(reg, i) === source
    );
    if (item) return { reg, item };
  }
  return null;
}

function registerStackRoutes(server) {
  server.get(
    '/rigger/registry/:name/stack',
    function ({ db }, { params, queryParams: q }) {
      const reg = registryOf(db, params.name);
      if (!reg) return error(404, `registry "${params.name}" not found`);
      const item = itemOf(reg, q.name || '');
      if (!item || item.kind !== 'stack')
        return error(404, `stack ${q.name} is not in the registry`);
      const version = q.version || item.versions[0];
      if (!item.versions.includes(version))
        return error(404, `${q.name} has no version ${version}`);
      return json({
        Source: stackSource(reg, item),
        Metadata: {
          name: item.name,
          version,
          description: item.description,
          category: item.category,
          keywords: item.keywords,
        },
        Readme: item.readme || '',
        FormSchema: item.schema,
        Versions: item.versions,
        Meets: true,
        Unmet: [],
        Warnings: [],
      });
    }
  );

  server.put('/rigger/stack/:action', function ({ db }, request) {
    const payload = body(request);
    const ns = request.queryParams.namespace || 'default';
    const found = publishedStack(db, payload.Source);
    if (!found) return error(404, `no stack at ${payload.Source}`);
    const { item } = found;
    const version = payload.Version || item.versions[0];
    const owned = (r) => {
      const o = r.Spec?.owner;
      return (
        o && o.kind === 'stack' && o.namespace === ns && o.name === item.name
      );
    };
    const releases = collection(db, 'norcaReleases').where(owned);
    const action = request.params.action;

    if (action === 'plan') {
      const values = payload.ValuesJSON || {};
      const missing = (item.schema?.required || []).filter(
        (k) => !(k in values)
      );
      if (missing.length)
        return error(400, `variable ${missing.join(', ')} has no value`);
      return json({
        Plan: {
          stack: item.name,
          region: 'global',
          releases: item.releases.map((r) => ({
            release: { namespace: ns, name: r.name },
            action: releases.some((x) => x.Name === r.name)
              ? 'upgrade'
              : 'install',
            module: { source: r.module, version: '1.0.0' },
          })),
          outputs: [],
        },
        Warnings: [],
      });
    }
    if (action === 'status') {
      return json({
        Stack: item.name,
        Releases: releases.map((r) => ({
          revision: {
            release: { namespace: ns, name: r.Name },
            number: r.Spec.revision,
          },
          drift:
            r.Name === 'site-db'
              ? [{ kind: 'job', name: r.Name, detail: 'count 1 → 2' }]
              : [],
        })),
      });
    }
    if (action !== 'apply' && action !== 'destroy')
      return error(404, `unknown stack action ${action}`);

    const running = collection(db, 'norcaObjects').where(
      (o) =>
        o.Kind === OPERATION_KIND &&
        o.Spec.stack === item.name &&
        o.Status.phase === 'Running'
    );
    if (running.length)
      return error(409, `stack ${item.name} has an operation running`);
    const done = item.releases.map((r) => ({ ...r }));
    if (action === 'apply') {
      for (const r of done) {
        r.revision = insertStackRelease(
          db,
          ns,
          item.name,
          payload.Source,
          version,
          r
        );
      }
    } else {
      for (const r of releases) collection(db, 'norcaReleases').remove(r.id);
    }
    const n = collection(db, 'norcaObjects').where(
      (o) => o.Kind === OPERATION_KIND
    ).length;
    const op = insertOperation(db, ns, `op-${item.name}-${n + 1}`, {
      stack: item.name,
      action,
      source: payload.Source,
      version,
      phase: 'Succeeded',
      startedAt: new Date().toISOString(),
      releases: done,
    });
    return json(operationView(op));
  });

  server.get('/rigger/operation/:name/log', function ({ db }, request) {
    const q = request.queryParams;
    const op = operationOf(db, request.params.name, q.namespace);
    if (!op) return error(404, `operation ${request.params.name} not found`);
    const after = Number(q.after || 0);
    return json({
      Lines: (op.Log || [])
        .map((line, i) => ({ Seq: i + 1, Line: line }))
        .filter((l) => l.Seq > after),
      Phase: op.Status.phase,
      Finished: op.Status.phase !== 'Running',
    });
  });

  server.put('/rigger/operation/:name/cancel', function ({ db }, request) {
    const op = operationOf(
      db,
      request.params.name,
      request.queryParams.namespace
    );
    if (!op) return error(404, `operation ${request.params.name} not found`);
    if (op.Status.phase !== 'Running')
      return error(409, `operation ${op.Name} has finished`);
    const status = { ...op.Status, phase: 'Cancelled' };
    collection(db, 'norcaObjects').update(op.id, { Status: status });
    return json(operationView({ ...op, Status: status }));
  });
}

function registryOf(db, name) {
  return collection(db, 'norcaObjects').findBy({
    Group: GROUP,
    Kind: KIND,
    Name: name,
  });
}

function itemOf(reg, address) {
  const [a, b] = address.split('/');
  const namespace = b ? a : '';
  const name = b || a;
  return (MODULES[reg.Spec.source] || []).find(
    (i) => i.namespace === namespace && i.name === name
  );
}

export default function registerRiggerRoutes(server) {
  server.put('/rigger/registry/:name/sync', function ({ db }, { params }) {
    const reg = registryOf(db, params.name);
    if (!reg) return error(404, `registry "${params.name}" not found`);
    if (!MODULES[reg.Spec.source]) {
      const status = {
        ...reg.Status,
        phase: 'Error',
        message: `fetching ${reg.Spec.source}: repository not found`,
        lastSyncedAt: new Date().toISOString(),
      };
      collection(db, 'norcaObjects').update(reg.id, { Status: status });
      return error(502, status.message);
    }
    const status = syncedStatus(reg.Spec, new Date().toISOString());
    collection(db, 'norcaObjects').update(reg.id, {
      Status: status,
      ModifyIndex: reg.ModifyIndex + 1,
    });
    return json({ name: reg.Name, spec: reg.Spec, status });
  });

  server.get(
    '/rigger/registry/:name/search',
    function ({ db }, { params, queryParams: q }) {
      const reg = registryOf(db, params.name);
      if (!reg) return error(404, `registry "${params.name}" not found`);
      if (!reg.Status?.lastGoodSyncAt) {
        return error(
          409,
          `the registry "${params.name}" has not been synced: sync it first`
        );
      }
      if (
        q.category &&
        ![
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
        ].includes(q.category)
      ) {
        return error(400, `invalid query: category "${q.category}"`);
      }
      const text = (q.text || '').toLowerCase();
      const found = (MODULES[reg.Spec.source] || []).filter(
        (i) =>
          (!q.kind || i.kind === q.kind) &&
          (!q.category || i.category === q.category) &&
          (!q.keyword || (i.keywords || []).includes(q.keyword)) &&
          (!text ||
            `${i.namespace}/${i.name} ${i.description || ''}`
              .toLowerCase()
              .includes(text))
      );
      const offset = Number(q.offset || 0);
      const limit = Number(q.limit || 50);
      return json({
        Items: found.slice(offset, offset + limit).map(strip),
        Total: found.length,
        Commit: reg.Status.commit,
      });
    }
  );

  server.get(
    '/rigger/registry/:name/module',
    function ({ db }, { params, queryParams: q }) {
      const reg = registryOf(db, params.name);
      if (!reg) return error(404, `registry "${params.name}" not found`);
      const item = itemOf(reg, q.name || '');
      if (!item) return error(404, `${q.name} is not in the registry`);
      const version = q.version || item.versions[0];
      if (!item.versions.includes(version))
        return error(404, `${q.name} has no version ${version}`);
      return json({
        Metadata: {
          name: item.name,
          version,
          description: item.description,
          category: item.category,
          license: item.license,
          keywords: item.keywords,
        },
        Readme: item.readme || '',
        FormSchema: item.schema,
        Versions: item.versions,
        Meets: !item.unmet,
        Unmet: item.unmet || [],
        Icon: Boolean(item.icon),
        Origin: {
          source: q.name,
          version,
          digest: `sha256:${item.name}${version}`,
        },
      });
    }
  );

  server.get('/rigger/registry/:name/icon', function () {
    return new Response(200, { 'Content-Type': 'image/png' }, 'PNG');
  });

  server.put('/rigger/install', function ({ db }, request) {
    const payload = body(request);
    const q = request.queryParams;
    const reg = registryOf(db, payload.Registry);
    if (!reg) return error(404, `registry "${payload.Registry}" not found`);
    const item = itemOf(reg, payload.Module || '');
    if (!item) return error(404, `${payload.Module} is not in the registry`);
    if (payload.Values && payload.ValuesJSON)
      return error(400, 'give the values as HCL or as JSON: one of them');
    const values = payload.ValuesJSON || {};
    const missing = (item.schema?.required || []).filter((k) => !(k in values));
    if (missing.length)
      return error(400, `variable ${missing.join(', ')} has no value`);

    const ns = q.namespace || 'default';
    const name = payload.Release;
    const c = collection(db, 'norcaReleases');
    const existing = c.where(
      (r) => r.Name === name && (r.Namespace || 'default') === ns
    )[0];
    const foreign = existing && existing.Spec?.pack?.manager !== 'rigger';
    const warnings = [];
    if (foreign)
      warnings.push(
        `the release ${name} belongs to pack: an apply takes it over only with adopt (the flag -adopt)`
      );
    const changes = [
      { kind: 'job', name, action: existing ? 'update' : 'create' },
    ];
    const n = (existing?.Spec.revision || 0) + 1;
    if (q.plan === 'true') {
      return json({
        Action: existing ? 'upgrade' : 'install',
        Changes: changes,
        Warnings: warnings,
        Revision: existing?.Spec.revision || 0,
      });
    }
    if (foreign && !payload.Adopt) {
      return error(
        409,
        `release "${name}" is managed by pack: rigger takes it over only when asked to (adopt; the flag -adopt)`
      );
    }
    const version = payload.Version || item.versions[0];
    // As the servers record it: the registry of the cluster, the source of
    // the module, and no sensitive value in the revision.
    const pack = {
      name: item.name,
      version,
      registry: reg.Name,
      source:
        reg.Spec.type === 'global'
          ? payload.Module
          : `${reg.Name}/${item.name}`,
      manager: 'rigger',
      digest: `sha256:${item.name}${version}`,
    };
    const sensitive = Object.entries(item.schema?.properties || {})
      .filter(([, p]) => p['x-ui-secret'])
      .map(([k]) => k);
    const recorded = Object.fromEntries(
      Object.entries(values).filter(([k]) => !sensitive.includes(k))
    );
    const record = {
      Region: q.region || 'global',
      ID: existing?.ID || `rel-${name}`,
      Namespace: ns,
      Name: name,
      Spec: { pack, revision: n, maxHistory: 10 },
      Status: {
        phase: 'deployed',
        revision: n,
        resources: [{ type: 'job', namespace: ns, name, hook: '' }],
        conditions: [],
      },
      History: [
        revision(ns, name, n, version, {
          spec: { pack, values: recorded },
          status: { phase: 'deployed', hooks: [] },
        }),
        ...(existing?.History || []),
      ],
      Events: [],
      Drift: [],
    };
    if (existing) c.update(existing.id, record);
    else c.insert(record);
    return json({
      Action: existing ? 'upgrade' : 'install',
      Changes: changes,
      Warnings: warnings,
      Revision: n,
      // The output next_steps of the module, as the servers answer an
      // apply.
      NextSteps: `${name} runs in namespace ${ns}.\n  norca service info ${name}\n`,
    });
  });

  registerStackRoutes(server);
}

export function riggerRegistry(db, name) {
  return plain(registryOf(db, name));
}
