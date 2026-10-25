/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, plain, error, json, inNamespace } from './util';

// Mirage routes of object kinds (norcaObjectKinds: ObjectKind JSON),
// custom objects (norcaObjects: CustomObject JSON plus Events) and
// admission policies (norcaAdmissionPolicies).

function kindOf(db, params) {
  return collection(db, 'norcaObjectKinds').findBy({
    Group: params.group,
    Kind: params.kind,
  });
}

function objectOf(db, params, queryParams) {
  const kind = kindOf(db, params);
  const namespace = kind?.Namespaced ? queryParams.namespace || 'default' : '';
  return collection(db, 'norcaObjects').where(
    (o) =>
      o.Group === params.group &&
      o.Kind === params.kind &&
      o.Name === params.name &&
      (o.Namespace || '') === namespace
  )[0];
}

function view(o) {
  // eslint-disable-next-line no-unused-vars
  const { Events, ...rest } = plain(o);
  return rest;
}

export default function registerObjectRoutes(server) {
  server.get('/object-kinds', function ({ db }) {
    return json(collection(db, 'norcaObjectKinds').map(plain));
  });

  server.get('/object-kind/:group/:kind', function ({ db }, { params }) {
    const k = kindOf(db, params);
    return k ? json(plain(k)) : error(404, 'object kind not found');
  });

  server.get(
    '/objects/:group/:kind',
    function ({ db }, { params, queryParams }) {
      const k = kindOf(db, params);
      if (!k) return error(404, 'object kind not found');
      const namespace = queryParams.namespace || 'default';
      return json(
        collection(db, 'norcaObjects')
          .where(
            (o) =>
              o.Group === params.group &&
              o.Kind === params.kind &&
              (!k.Namespaced || inNamespace(o, namespace))
          )
          .map(view)
      );
    }
  );

  server.get(
    '/object/:group/:kind/:name/events',
    function ({ db }, { params, queryParams }) {
      const o = objectOf(db, params, queryParams);
      return o ? json(o.Events || []) : error(404, 'object not found');
    }
  );

  server.get(
    '/object/:group/:kind/:name',
    function ({ db }, { params, queryParams }) {
      const o = objectOf(db, params, queryParams);
      return o ? json(view(o)) : error(404, 'object not found');
    }
  );

  server.get('/admission-policies', function ({ db }) {
    return json(collection(db, 'norcaAdmissionPolicies').map(plain));
  });

  server.get('/admission-policy/:name', function ({ db }, { params }) {
    const p = collection(db, 'norcaAdmissionPolicies').findBy({
      Name: params.name,
    });
    return p ? json(plain(p)) : error(404, 'admission policy not found');
  });

  server.delete('/admission-policy/:name', function ({ db }, { params }) {
    const c = collection(db, 'norcaAdmissionPolicies');
    const p = c.findBy({ Name: params.name });
    if (!p) return error(404, `admission policy "${params.name}" not found`);
    c.remove(p.id);
    return json(null);
  });
}

const SCHEMA = JSON.stringify({
  type: 'object',
  properties: {
    spec: {
      type: 'object',
      required: ['version', 'replicas'],
      properties: {
        version: { type: 'string', enum: ['15', '16'] },
        replicas: { type: 'integer', minimum: 1, maximum: 5 },
        storage: { type: 'string', default: '10GiB' },
      },
    },
    status: { type: 'object' },
  },
});

function condition(type, status, reason, message = '') {
  return {
    type,
    status,
    reason,
    message,
    lastTransitionTime: new Date().toISOString(),
  };
}

/**
 * Seeds the PostgresCluster kind (owned by the postgres-operator release)
 * with three objects, a cluster-scoped kind without objects, and two
 * admission policies.
 */
export function seedObjects(server) {
  const db = server.db;
  collection(db, 'norcaObjectKinds').insert({
    Group: 'db.example.com',
    Kind: 'PostgresCluster',
    Description: 'A replicated Postgres cluster',
    Namespaced: true,
    Schema: SCHEMA,
    Rules: [
      {
        Expression:
          'self.spec.replicas >= oldSelf.spec.replicas || self.spec.replicas >= 2',
        Message: 'replicas cannot drop below 2',
      },
    ],
    Columns: [
      { Name: 'Version', Path: 'spec.version' },
      { Name: 'Replicas', Path: 'status.readyReplicas' },
      { Name: 'Ready', Path: "status.conditions[?type=='Ready'].status" },
    ],
    Scale: { SpecPath: 'spec.replicas', StatusPath: 'status.readyReplicas' },
    Replicated: false,
    Version: 2,
    Migration: null,
    Webhooks: [
      {
        Name: 'defaults',
        Mode: 'default',
        URL: 'https://pg-operator.platform:8443/default',
        FailurePolicy: 'fail',
        Operations: ['create', 'update'],
        Timeout: 5e9,
        Approval: {
          Digest: 'sha256:1',
          ApprovedBy: 'token:abc',
          ApproveTime: Date.now() * 1e6,
        },
      },
    ],
    Limits: { MaxObjectsPerNamespace: 50, WriteRate: 5, WriteBurst: 10 },
    CreateIndex: 1,
    ModifyIndex: 5,
  });
  collection(db, 'norcaObjectKinds').insert({
    Group: 'backup.example.com',
    Kind: 'Schedule',
    Description: '',
    Namespaced: false,
    Schema: '',
    Rules: null,
    Columns: null,
    Scale: null,
    Version: 1,
    Webhooks: null,
    Limits: null,
    CreateIndex: 2,
    ModifyIndex: 2,
  });
  const objects = collection(db, 'norcaObjects');
  const base = {
    Group: 'db.example.com',
    Kind: 'PostgresCluster',
    Labels: { team: 'billing' },
    ManagedBy: '',
    Owners: null,
    Finalizers: ['db.example.com/backup'],
    DeletionTime: 0,
    KindVersion: 2,
    CreateTime: (Date.now() - 86400000) * 1e6,
    ModifyTime: Date.now() * 1e6,
    CreateIndex: 10,
    ModifyIndex: 20,
  };
  objects.insert({
    ...base,
    ID: 'obj-1',
    Namespace: 'default',
    Name: 'billing',
    Generation: 7,
    Spec: { version: '16', replicas: 3, storage: '100GiB' },
    Status: {
      observedGeneration: 7,
      readyReplicas: 3,
      primary: 'billing-db-0',
      conditions: [
        condition(
          'Ready',
          'True',
          'AllReplicasReady',
          'every replica is ready'
        ),
        condition('Progressing', 'False', 'Stable'),
        condition('Degraded', 'False', 'Healthy'),
      ],
    },
    Events: [
      {
        Type: 'Normal',
        Reason: 'BackupSucceeded',
        Message: 'billing-20261024-0200, 3.2 GB',
        Reporter: 'pg-operator',
        Count: 1,
        FirstTime: Date.now() * 1e6,
        LastTime: Date.now() * 1e6,
      },
      {
        Type: 'Normal',
        Reason: 'Scaled',
        Message: 'replicas 2 → 3',
        Reporter: 'norca',
        Count: 1,
        FirstTime: (Date.now() - 3600000) * 1e6,
        LastTime: (Date.now() - 3600000) * 1e6,
      },
    ],
  });
  objects.insert({
    ...base,
    ID: 'obj-2',
    Namespace: 'default',
    Name: 'reports',
    Generation: 4,
    Spec: { version: '16', replicas: 2 },
    Status: {
      observedGeneration: 3,
      readyReplicas: 1,
      conditions: [
        condition('Ready', 'False', 'ReplicaStarting', '1 of 2 replicas ready'),
        condition('Progressing', 'True', 'Scaling'),
      ],
    },
    Events: [
      {
        Type: 'Warning',
        Reason: 'ReplicaFailed',
        Message: 'replica reports-1 crashed',
        Reporter: 'pg-operator',
        Count: 3,
        FirstTime: Date.now() * 1e6,
        LastTime: Date.now() * 1e6,
      },
    ],
  });
  objects.insert({
    ...base,
    ID: 'obj-3',
    Namespace: 'staging',
    Name: 'test-db',
    Generation: 2,
    Owners: [
      {
        Kind: 'packs.norca.io/Release',
        Namespace: 'staging',
        Name: 'test-stack',
        ID: 'r-9',
      },
    ],
    Spec: { version: '15', replicas: 1 },
    Status: {
      observedGeneration: 2,
      readyReplicas: 1,
      conditions: [condition('Ready', 'True', 'AllReplicasReady')],
    },
    Events: [],
  });

  const policies = collection(db, 'norcaAdmissionPolicies');
  policies.insert({
    Name: 'no-privileged-containers',
    Description: 'Privileged containers are not allowed in production',
    Action: 'deny',
    Match: { Namespaces: ['prod*'], Resources: ['job'], VariablePaths: null },
    SchemaFrom: '',
    Rules: [
      {
        Expression:
          "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))",
        Message:
          'Privileged containers are not allowed in the prod namespaces.',
      },
    ],
    TrustedKeys: null,
    CreateIndex: 30,
    ModifyIndex: 31,
  });
  policies.insert({
    Name: 'team-label',
    Description: '',
    Action: 'warn',
    Match: null,
    SchemaFrom: '',
    Rules: [{ Expression: 'has(job.Meta.team)', Message: '' }],
    TrustedKeys: null,
    CreateIndex: 32,
    ModifyIndex: 32,
  });
}
