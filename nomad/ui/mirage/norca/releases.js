/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, body, plain, error, json, inNamespace } from './util';

// Mirage routes of the pack release API. norcaReleases holds Release JSON
// plus Region, History ([ReleaseRevision], newest first), Events (the object
// event log of the release) and Drift (the drift entries a check finds).
// The typed views use camelCase keys inside Spec and Status, as an agent
// built without the codegen tag does; the UI normalizes both casings.

function find(db, request) {
  const namespace = request.queryParams.namespace || 'default';
  const region = request.queryParams.region;
  return collection(db, 'norcaReleases').where(
    (r) =>
      r.Name === request.params.name &&
      (r.Namespace || 'default') === namespace &&
      (!region || !r.Region || r.Region === region)
  )[0];
}

function view(r) {
  // eslint-disable-next-line no-unused-vars
  const { Region, History, Events, Drift, ...rest } = plain(r);
  return rest;
}

export default function registerReleaseRoutes(server) {
  server.get('/releases', function ({ db }, request) {
    const namespace = request.queryParams.namespace || 'default';
    const region = request.queryParams.region;
    const prefix = request.queryParams.prefix || '';
    return json(
      collection(db, 'norcaReleases')
        .where(
          (r) =>
            inNamespace(r, namespace) &&
            r.Name.startsWith(prefix) &&
            (!region || !r.Region || r.Region === region)
        )
        .map(view)
    );
  });

  server.get('/release/:name/history', function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    return json(
      (r.History || []).map((rev) => ({
        ...rev,
        Spec: { ...rev.Spec, content: '' },
        Resources: null,
      }))
    );
  });

  server.get('/release/:name/revision/:n', function ({ db }, request) {
    const r = find(db, request);
    const rev = (r?.History || []).find(
      (h) => String(h.Revision) === request.params.n
    );
    if (!rev) return error(404, 'revision not found');
    return json(rev);
  });

  const rollback = function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    const payload = body(request);
    const history = r.History || [];
    const target =
      history.find((h) => h.Revision === payload.Revision) ||
      history.find(
        (h) =>
          h.Revision < r.Spec.revision &&
          ['deployed', 'superseded'].includes(h.Status.phase)
      );
    if (!target) return error(404, 'revision not found');
    const next = r.Spec.revision + 1;
    const changes = [
      {
        Type: 'job',
        Namespace: r.Namespace,
        Name: r.Name,
        Action: 'update',
        Hook: '',
        Owner: '',
        JobDiff: null,
        Region: '',
        Error: '',
      },
    ];
    if (!payload.DryRun) {
      const revision = {
        Namespace: r.Namespace,
        Release: r.Name,
        Revision: next,
        Spec: {
          ...target.Spec,
          revision: next,
          action: 'rollback',
          rollbackOf: target.Revision,
          createTime: new Date().toISOString(),
        },
        Status: { phase: 'deployed', message: '', hooks: [] },
      };
      collection(db, 'norcaReleases').update(r.id, {
        Spec: { ...r.Spec, revision: next, pack: target.Spec.pack },
        Status: {
          ...r.Status,
          phase: 'deployed',
          revision: next,
          lastOperation: {
            action: 'rollback',
            revision: next,
            user: 'mirage',
            startTime: new Date().toISOString(),
            endTime: new Date().toISOString(),
          },
        },
        History: [
          revision,
          ...history.map((h) =>
            h.Status.phase === 'deployed'
              ? { ...h, Status: { ...h.Status, phase: 'superseded' } }
              : h
          ),
        ],
      });
    }
    return json({
      Release: payload.DryRun ? null : view(find(db, request)),
      Revision: payload.DryRun ? null : next,
      Changes: changes,
      Warnings: null,
      Regions: null,
      Index: 1,
    });
  };
  server.post('/release/:name/rollback', rollback);
  server.put('/release/:name/rollback', rollback);

  const runTest = function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    const now = new Date().toISOString();
    const result = {
      revision: r.Status.revision,
      startTime: now,
      endTime: now,
      succeeded: true,
      hooks: [
        {
          phase: 'test',
          namespace: r.Namespace,
          job: `${r.Name}-connection-test`,
          status: 'succeeded',
          message: '',
        },
      ],
    };
    collection(db, 'norcaReleases').update(r.id, {
      Status: { ...r.Status, lastTest: result },
    });
    return json(result);
  };
  server.post('/release/:name/test', runTest);
  server.put('/release/:name/test', runTest);

  const drift = function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    const entries = r.Drift || [];
    return json({
      Drifted: entries.length > 0,
      Revision: r.Status.revision,
      Resources: entries,
      Index: 1,
    });
  };
  server.get('/release/:name/drift', drift);
  server.post('/release/:name/drift', drift);
  server.put('/release/:name/drift', drift);

  server.get('/release/:name', function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    return json(view(r));
  });

  server.delete('/release/:name', function ({ db }, request) {
    const r = find(db, request);
    if (!r) return error(404, 'release not found');
    collection(db, 'norcaReleases').remove(r.id);
    return json({
      Release: view(r),
      Changes: (r.Status.resources || []).map((res) => ({
        Type: res.type,
        Namespace: res.namespace,
        Name: res.name,
        Action: 'delete',
      })),
      Index: 1,
    });
  });

  server.get(
    '/object/packs.norca.io/Release/:name/events',
    function ({ db }, request) {
      const r = find(db, request);
      if (!r) return error(404, 'object not found');
      return json(r.Events || []);
    }
  );
}

export function revision(ns, name, n, version, extra = {}) {
  return {
    Namespace: ns,
    Release: name,
    Revision: n,
    Spec: {
      release: name,
      revision: n,
      action: n === 1 ? 'install' : 'upgrade',
      rollbackOf: 0,
      pack: {
        name,
        version,
        registry: 'https://packs.example.com',
        digest: `sha256:${n}abc`,
      },
      description: '',
      values: {
        replicas: n > 2 ? 3 : 2,
        image: `registry.example.com/${name}:${version}`,
      },
      valuesHash: `h${n}`,
      variablesSchemas: [
        {
          pack: name,
          schema: JSON.stringify({
            type: 'object',
            required: ['image'],
            properties: {
              replicas: { type: 'integer', minimum: 1, maximum: 5 },
              image: { type: 'string', minLength: 1 },
            },
          }),
        },
      ],
      resources: [
        { type: 'namespace', namespace: '', name: 'db-operator', hash: 'n1' },
        { type: 'job', namespace: ns, name, hash: `j${n}` },
      ],
      user: 'alice',
      createTime: new Date(Date.now() - (10 - n) * 86400000).toISOString(),
      ...extra.spec,
    },
    Status: {
      phase: 'superseded',
      message: '',
      hooks: [
        {
          phase: 'pre-upgrade',
          namespace: ns,
          job: `${name}-migrate`,
          status: 'succeeded',
          message: '',
        },
      ],
      ...extra.status,
    },
  };
}

/**
 * Seeds the postgres-operator release (deployed, revision 3, signed,
 * multi-region) and observability (drifted, unsigned).
 */
export function seedReleases(server, { region = 'global' } = {}) {
  const c = collection(server.db, 'norcaReleases');
  c.insert({
    Region: region,
    ID: 'rel-1',
    Namespace: 'platform',
    Name: 'postgres-operator',
    Spec: {
      pack: {
        name: 'postgres-operator',
        version: '1.4.0',
        registry: 'https://packs.example.com',
        digest: 'sha256:3abc',
        signer: 'key-1',
        trustedSigner: 'key-1',
      },
      description: 'Postgres operator',
      revision: 3,
      maxHistory: 10,
      atomic: true,
      multiregion: { regions: [region], globalRegion: region },
    },
    Status: {
      phase: 'deployed',
      revision: 3,
      resources: [
        { type: 'namespace', namespace: '', name: 'db-operator', hook: '' },
        {
          type: 'acl-policy',
          namespace: '',
          name: 'postgres-operator-controller',
          hook: '',
        },
        {
          type: 'object-kind',
          namespace: '',
          name: 'db.example.com/PostgresCluster',
          hook: '',
        },
        {
          type: 'job',
          namespace: 'platform',
          name: 'postgres-operator',
          hook: '',
        },
        {
          type: 'job',
          namespace: 'platform',
          name: 'postgres-operator-migrate',
          hook: 'pre-upgrade',
        },
      ],
      conditions: [
        {
          type: 'Ready',
          status: 'True',
          reason: 'Deployed',
          message: 'all resources applied',
          lastTransitionTime: new Date().toISOString(),
        },
        {
          type: 'HooksSucceeded',
          status: 'True',
          reason: 'HooksSucceeded',
          message: '',
          lastTransitionTime: new Date().toISOString(),
        },
        {
          type: 'Drifted',
          status: 'False',
          reason: 'InSync',
          message: '',
          lastTransitionTime: new Date().toISOString(),
        },
      ],
      lastOperation: {
        action: 'upgrade',
        revision: 3,
        user: 'alice',
        startTime: new Date().toISOString(),
        endTime: new Date().toISOString(),
      },
      lastTest: null,
    },
    CreateTime: Date.now() * 1e6,
    ModifyTime: Date.now() * 1e6,
    History: [
      revision('platform', 'postgres-operator', 3, '1.4.0', {
        status: { phase: 'deployed' },
      }),
      revision('platform', 'postgres-operator', 2, '1.3.2', {
        status: {
          phase: 'failed',
          message: 'post-upgrade hook failed; rolled back',
        },
      }),
      revision('platform', 'postgres-operator', 1, '1.3.0'),
    ],
    Events: [
      {
        Type: 'Normal',
        Reason: 'Deployed',
        Message: 'revision 3 deployed',
        Reporter: 'release',
        Count: 1,
        FirstTime: Date.now() * 1e6,
        LastTime: Date.now() * 1e6,
      },
    ],
    Drift: [],
  });
  c.insert({
    Region: region,
    ID: 'rel-2',
    Namespace: 'platform',
    Name: 'observability',
    Spec: {
      pack: {
        name: 'observability',
        version: '0.9.2',
        registry: '',
        digest: '',
      },
      description: '',
      revision: 1,
      maxHistory: 10,
      atomic: false,
    },
    Status: {
      phase: 'deployed',
      revision: 1,
      resources: [
        { type: 'job', namespace: 'platform', name: 'log-shipper', hook: '' },
      ],
      conditions: [
        {
          type: 'Drifted',
          status: 'True',
          reason: 'Drifted',
          message: 'job log-shipper was modified',
          lastTransitionTime: new Date().toISOString(),
        },
      ],
    },
    History: [
      revision('platform', 'observability', 1, '0.9.2', {
        status: { phase: 'deployed' },
      }),
    ],
    Events: [
      {
        Type: 'Warning',
        Reason: 'Drifted',
        Message: 'job log-shipper was modified',
        Reporter: 'release',
        Count: 2,
        FirstTime: Date.now() * 1e6,
        LastTime: Date.now() * 1e6,
      },
    ],
    Drift: [
      {
        type: 'job',
        namespace: 'platform',
        name: 'log-shipper',
        reason: 'modified',
      },
    ],
  });
}
