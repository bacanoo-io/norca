/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, body, plain, error, json } from './util';
import { revision } from './releases';

// Mirage routes of the cluster pack registries and catalog
// (nomad/command/agent/pack_endpoint.go). norcaPackRegistries holds
// PackRegistry JSON: Spec and Status keep the camelCase keys of the Go
// types. A sync takes the packs of the source from SOURCES; a source that
// is not there fails as an unreachable registry does.

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
      type: 'number',
      title: 'Replicas',
      minimum: 1,
      maximum: 9,
      default: 2,
      'x-ui-tab': 'resources',
      'x-ui-group': 'Capacity & Scaling',
      'x-ui-widget': 'slider',
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
      'x-ui-widget': 'secret',
      'x-ui-secret-path': 'nomad/jobs/redis',
      'x-ui-secret-item': 'password',
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

// The packs of each source: catalog entry, README and form schema.
export const SOURCES = {
  builtin: [
    {
      entry: {
        Name: 'redis',
        Version: '7.2.4',
        Description:
          'Redis with optional replicas and a password in a Variable',
        Category: 'database',
        Digest: 'sha256:redis724',
        Signed: true,
        Signer: 'norca-builtin',
        HasSchema: true,
        Requires: { norca: '>= 0.1.0', features: ['releases'] },
      },
      readme: '# Redis\n\nIn-memory data store.',
      schema: REDIS_SCHEMA,
    },
    {
      entry: {
        Name: 'redis',
        Version: '7.0.15',
        Description:
          'Redis with optional replicas and a password in a Variable',
        Category: 'database',
        Digest: 'sha256:redis7015',
        Signed: true,
        Signer: 'norca-builtin',
        HasSchema: true,
      },
      readme: '# Redis 7.0',
      schema: REDIS_SCHEMA,
    },
    {
      entry: {
        Name: 'loadbalancer-operator',
        Version: '0.3.0',
        Description: 'Ingress operator with LoadBalancer objects',
        Category: 'networking',
        Digest: 'sha256:lb030',
        HasSchema: true,
        Requires: { norca: '>= 9.0.0', features: ['custom-objects'] },
        Compatible: false,
        MissingFeatures: ['requires Norca >= 9.0.0'],
      },
      readme: '',
      schema: { type: 'object', properties: {} },
    },
  ],
  'oci://ghcr.io/example/packs': [
    {
      entry: {
        Name: 'web-app',
        Version: '1.2.0',
        Description: 'A stateless web service',
        Category: 'application',
        Digest: 'sha256:web120',
        HasSchema: true,
      },
      readme: '# Web app',
      schema: APP_SCHEMA,
    },
  ],
};

// A failed sync keeps the packs of the last good one, as the servers do.
function sync(name, spec, prev) {
  const packs = SOURCES[spec.source];
  const now = new Date().toISOString();
  if (!packs) {
    return {
      phase: 'Error',
      message: `failed to fetch ${spec.source}: not found`,
      lastSyncedAt: now,
      lastGoodSyncAt: prev?.lastGoodSyncAt,
      syncGeneration: prev?.syncGeneration || 0,
      packCount: prev?.packCount || 0,
      packs: prev?.packs,
    };
  }
  return {
    phase: 'Ready',
    message: `Synchronized ${packs.length} pack(s)`,
    lastSyncedAt: now,
    lastGoodSyncAt: now,
    syncGeneration: (prev?.syncGeneration || 0) + 1,
    packCount: packs.length,
    packs: packs.map((p) => ({ Compatible: true, ...p.entry, Registry: name })),
  };
}

function registryByName(db, name) {
  return collection(db, 'norcaPackRegistries').findBy({ Name: name });
}

// The registry view without the indexed packs, as the list returns it
// with them; the UI reads the count.
function view(reg) {
  return plain(reg);
}

function packOf(reg, name, version) {
  const packs = SOURCES[reg.Spec.source] || [];
  const all = packs.filter((p) => p.entry.Name === name);
  const match = version ? all.find((p) => p.entry.Version === version) : all[0];
  return match ? { match, versions: all.map((p) => p.entry.Version) } : null;
}

const NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

export default function registerPackRoutes(server) {
  server.get('/pack-registries', function ({ db }) {
    return json(collection(db, 'norcaPackRegistries').all().map(view));
  });

  server.post('/pack-registry', function ({ db }, request) {
    const payload = body(request);
    const spec = payload.Spec;
    if (!spec) return error(400, 'missing registry spec');
    if (!NAME.test(payload.Name || '')) {
      return error(400, `invalid registry name: ${payload.Name}`);
    }
    if (!['dir', 'git', 'oci', 'bundle'].includes(spec.type)) {
      return error(
        400,
        `invalid registry type "${spec.type}": must be dir, git, oci or bundle`
      );
    }
    if (!spec.source) return error(400, 'registry source is required');
    const c = collection(db, 'norcaPackRegistries');
    const existing = c.findBy({ Name: payload.Name });
    const record = {
      ID: existing?.ID || `reg-${payload.Name}`,
      Name: payload.Name,
      Spec: {
        type: spec.type,
        source: spec.source,
        defaultRef: spec.defaultRef || 'latest',
        ...(spec.syncInterval ? { syncInterval: spec.syncInterval } : {}),
        ...(spec.trustedKeys?.length ? { trustedKeys: spec.trustedKeys } : {}),
      },
      Status: sync(payload.Name, spec, existing?.Status),
    };
    if (existing) c.update(existing.id, record);
    else c.insert(record);
    return json(record);
  });

  server.post('/pack-registry/:name/sync', function ({ db }, request) {
    const reg = registryByName(db, request.params.name);
    if (!reg)
      return error(404, `pack registry "${request.params.name}" not found`);
    const Status = sync(reg.Name, reg.Spec, reg.Status);
    collection(db, 'norcaPackRegistries').update(reg.id, { Status });
    return json({ ...view(reg), Status });
  });

  server.del('/pack-registry/:name', function ({ db }, request) {
    const reg = registryByName(db, request.params.name);
    if (!reg)
      return error(404, `pack registry "${request.params.name}" not found`);
    collection(db, 'norcaPackRegistries').remove(reg.id);
    return json({ Index: 2 });
  });

  server.get('/packs', function ({ db }, request) {
    const filter = request.queryParams.registry;
    const out = [];
    for (const reg of collection(db, 'norcaPackRegistries').all()) {
      if (filter && reg.Name !== filter) continue;
      out.push(...(reg.Status?.packs || []));
    }
    return json(out);
  });

  server.get('/pack/:registry/:pack', function ({ db }, request) {
    const reg = registryByName(db, request.params.registry);
    if (!reg)
      return error(404, `pack registry "${request.params.registry}" not found`);
    const found = packOf(reg, request.params.pack, request.queryParams.version);
    if (!found) {
      return error(
        404,
        `pack "${request.params.pack}" (version "${
          request.queryParams.version || ''
        }") not found in registry "${reg.Name}"`
      );
    }
    return json({
      Pack: { Compatible: true, ...found.match.entry, Registry: reg.Name },
      Versions: found.versions,
      Readme: found.match.readme,
      UIFormSchema: found.match.schema,
    });
  });

  function apply(db, request, dryRun) {
    const payload = body(request);
    if (!payload.Registry || !payload.Pack)
      return error(400, 'registry and pack are required');
    if (!NAME.test(request.params.name))
      return error(400, `invalid release name "${request.params.name}"`);
    const reg = registryByName(db, payload.Registry);
    if (!reg)
      return error(404, `pack registry "${payload.Registry}" not found`);
    const found = packOf(reg, payload.Pack, payload.Version);
    if (!found)
      return error(
        404,
        `pack "${payload.Pack}" not found in registry "${reg.Name}"`
      );
    const missing = (found.match.schema.required || []).filter(
      (k) => !(k in (payload.Values || {}))
    );
    if (missing.length)
      return error(
        400,
        `pack render failed: missing variable ${missing.join(', ')}`
      );

    const ns = request.queryParams.namespace || 'default';
    const region = request.queryParams.region || 'global';
    const name = request.params.name;
    const c = collection(db, 'norcaReleases');
    const existing = c.where(
      (r) =>
        r.Name === name &&
        (r.Namespace || 'default') === ns &&
        (!r.Region || r.Region === region)
    )[0];
    const Changes = [
      {
        Type: 'job',
        Namespace: ns,
        Name: name,
        Action: existing ? 'update' : 'create',
        Hook: '',
        Owner: '',
      },
    ];
    if (dryRun)
      return json({
        Changes,
        Warnings: [],
        Revision: (existing?.Spec.revision || 0) + 1,
      });

    const n = (existing?.Spec.revision || 0) + 1;
    const pack = {
      name: payload.Pack,
      version: found.match.entry.Version,
      registry: reg.Name,
      digest: found.match.entry.Digest,
    };
    const spec = {
      pack,
      description: payload.Description || '',
      revision: n,
      maxHistory: 10,
      atomic: true,
    };
    const rev = revision(ns, name, n, pack.version, {
      spec: {
        pack,
        values: payload.Values || {},
        variablesSchemas: [
          { pack: payload.Pack, schema: JSON.stringify(found.match.schema) },
        ],
        resources: [{ type: 'job', namespace: ns, name, hash: `j${n}` }],
      },
      status: { phase: 'deployed', hooks: [] },
    });
    const record = {
      Region: region,
      ID: existing?.ID || `rel-${name}`,
      Namespace: ns,
      Name: name,
      Spec: spec,
      Status: {
        phase: 'deployed',
        revision: n,
        resources: [{ type: 'job', namespace: ns, name, hook: '' }],
        conditions: [
          {
            type: 'Ready',
            status: 'True',
            reason: 'Deployed',
            message: `revision ${n} deployed`,
            lastTransitionTime: new Date().toISOString(),
          },
        ],
      },
      History: [
        rev,
        ...(existing?.History || []).map((h) => ({
          ...h,
          Status: { ...h.Status, phase: 'superseded' },
        })),
      ],
      Events: existing?.Events || [],
      Drift: [],
    };
    if (existing) c.update(existing.id, record);
    else c.insert(record);
    // eslint-disable-next-line no-unused-vars
    const { Region, History, Events, Drift, ...release } = record;
    return json({ Release: release, Revision: n, Changes, Warnings: [] });
  }

  server.post('/release/:name/plan-pack', function ({ db }, request) {
    return apply(db, request, true);
  });

  server.post('/release/:name/deploy-pack', function ({ db }, request) {
    return apply(db, request, false);
  });
}

// The built-in packs as a registry.
export function seedPacks(server) {
  const spec = { type: 'dir', source: 'builtin', defaultRef: 'latest' };
  collection(server.db, 'norcaPackRegistries').insert({
    ID: 'reg-builtin',
    Name: 'builtin',
    Spec: spec,
    Status: sync('builtin', spec),
  });
}
