/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Response } from 'miragejs';
import { collection, body, plain, error, json } from './util';
import { revision } from './releases';
import { REDIS_SCHEMA, APP_SCHEMA } from './packs';

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

// The redis module of Rigger: the form of the redis pack, its password a
// sensitive value, and a TLS key the job reads from the secret store.
const RIGGER_REDIS_SCHEMA = (() => {
  const s = JSON.parse(JSON.stringify(REDIS_SCHEMA));
  s.properties.redis_password = {
    ...s.properties.redis_password,
    'x-ui-secret': true,
  };
  delete s.properties.redis_password['x-ui-widget'];
  delete s.properties.redis_password['x-ui-secret-path'];
  delete s.properties.redis_password['x-ui-secret-item'];
  s.properties.tls_key = {
    type: 'string',
    title: 'TLS key',
    'x-ui-tab': 'security',
    'x-ui-widget': 'secret',
    'x-ui-secret-path': 'nomad/jobs/redis',
    'x-ui-secret-item': 'tls_key',
  };
  return s;
})();

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
      schema: RIGGER_REDIS_SCHEMA,
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
  ],
};

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
    const pack = {
      name: item.name,
      version,
      source: payload.Module,
      manager: 'rigger',
      digest: `sha256:${item.name}${version}`,
    };
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
          spec: { pack, values },
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
    });
  });
}

export function riggerRegistry(db, name) {
  return plain(registryOf(db, name));
}
