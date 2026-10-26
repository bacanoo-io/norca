/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, body, plain, error, json, inNamespace } from './util';

// Mirage routes of the native service catalog with health
// (norcaServices: ServiceRegistration JSON), service intentions
// (norcaIntentions), network policies (norcaNetworkPolicies), admin network
// policies (norcaAdminNetworkPolicies) and the mTLS roots (norcaMtlsRoots).

export default function registerNetworkRoutes(server) {
  server.get('/services', function ({ db }, { queryParams }) {
    const namespace = queryParams.namespace || 'default';
    const byNs = new Map();
    collection(db, 'norcaServices')
      .where((s) => inNamespace(s, namespace))
      .forEach((s) => {
        const ns = s.Namespace || 'default';
        if (!byNs.has(ns)) byNs.set(ns, new Map());
        const services = byNs.get(ns);
        const tags = new Set(services.get(s.ServiceName) || []);
        (s.Tags || []).forEach((t) => tags.add(t));
        services.set(s.ServiceName, [...tags]);
      });
    return json(
      [...byNs.entries()].map(([ns, services]) => ({
        Namespace: ns,
        Services: [...services.entries()].map(([name, tags]) => ({
          ServiceName: name,
          Tags: tags,
        })),
      }))
    );
  });

  server.get('/service/:name', function ({ db }, { params, queryParams }) {
    const namespace = queryParams.namespace || 'default';
    return json(
      collection(db, 'norcaServices')
        .where(
          (s) => s.ServiceName === params.name && inNamespace(s, namespace)
        )
        .map(plain)
    );
  });

  server.get('/mtls/roots', function ({ db }) {
    const roots = collection(db, 'norcaMtlsRoots').map(plain);
    return json({
      TrustDomain: roots.length ? 'norca.local' : '',
      ActiveRootID: roots.find((r) => r.Active)?.ID || '',
      Roots: roots,
    });
  });

  // Intentions
  server.get('/service-intentions', function ({ db }, { queryParams }) {
    const namespace = queryParams.namespace || 'default';
    return json(
      collection(db, 'norcaIntentions')
        .where((i) => inNamespace(i, namespace))
        .map(plain)
    );
  });

  const key = (i) => ({
    Namespace: i.Namespace || 'default',
    Destination: i.Destination,
    SourceNamespace: i.SourceNamespace || i.Namespace || 'default',
    Source: i.Source,
  });

  const upsertIntention = function ({ db }, request) {
    const intention = body(request);
    if (!intention.Source || !intention.Destination)
      return error(400, 'source and destination are required');
    if (!['allow', 'deny'].includes(intention.Action))
      return error(400, `invalid action "${intention.Action}"`);
    const record = {
      ...intention,
      ...key({
        ...intention,
        Namespace: intention.Namespace || request.queryParams.namespace,
      }),
    };
    const intentions = collection(db, 'norcaIntentions');
    const existing = intentions.findBy(key(record));
    if (existing) intentions.update(existing.id, record);
    else intentions.insert({ ...record, CreateIndex: 1, ModifyIndex: 1 });
    return json(null);
  };
  server.put('/service-intention', upsertIntention);
  server.post('/service-intention', upsertIntention);
  server.put('/service-intentions', upsertIntention);

  server.delete('/service-intention', function ({ db }, { queryParams }) {
    const { source, destination } = queryParams;
    const namespace = queryParams.namespace || 'default';
    const intentions = collection(db, 'norcaIntentions');
    const existing = intentions.findBy(
      key({
        Source: source,
        Destination: destination,
        Namespace: namespace,
        SourceNamespace: queryParams.source_namespace,
      })
    );
    if (!existing) return error(404, 'intention not found');
    intentions.remove(existing.id);
    return json(null);
  });

  // Network policies
  server.get('/network-policies', function ({ db }, { queryParams }) {
    const namespace = queryParams.namespace || 'default';
    return json(
      collection(db, 'norcaNetworkPolicies')
        .where((p) => inNamespace(p, namespace))
        .map(plain)
    );
  });

  server.get('/network-policy/:name/status', function ({ db }, request) {
    const namespace = request.queryParams.namespace || 'default';
    const p = collection(db, 'norcaNetworkPolicies').findBy({
      Name: request.params.name,
      Namespace: namespace,
    });
    if (!p) return error(404, 'network policy not found');
    return json(p.Status || []);
  });

  server.get('/network-policy/:name', function ({ db }, request) {
    const namespace = request.queryParams.namespace || 'default';
    const p = collection(db, 'norcaNetworkPolicies').findBy({
      Name: request.params.name,
      Namespace: namespace,
    });
    if (!p) return error(404, 'network policy not found');
    // eslint-disable-next-line no-unused-vars
    const { Status, ...rest } = plain(p);
    return json(rest);
  });

  server.delete('/network-policy/:name', function ({ db }, request) {
    const namespace = request.queryParams.namespace || 'default';
    const policies = collection(db, 'norcaNetworkPolicies');
    const p = policies.findBy({
      Name: request.params.name,
      Namespace: namespace,
    });
    if (!p) return error(404, 'network policy not found');
    policies.remove(p.id);
    return json(null);
  });

  // Admin network policies
  server.get('/admin-network-policies', function ({ db }) {
    return json(
      collection(db, 'norcaAdminNetworkPolicies')
        .map(plain)
        .map((p) => {
          // eslint-disable-next-line no-unused-vars
          const { Status, ...rest } = p;
          return rest;
        })
        .sort((a, b) => a.Priority - b.Priority || (a.Name < b.Name ? -1 : 1))
    );
  });

  server.get('/admin-network-policy/:name/status', function ({ db }, request) {
    const p = collection(db, 'norcaAdminNetworkPolicies').findBy({
      Name: request.params.name,
    });
    if (!p) return error(404, 'admin network policy not found');
    return json(p.Status || []);
  });

  server.get('/admin-network-policy/:name', function ({ db }, request) {
    const p = collection(db, 'norcaAdminNetworkPolicies').findBy({
      Name: request.params.name,
    });
    if (!p) return error(404, 'admin network policy not found');
    // eslint-disable-next-line no-unused-vars
    const { Status, ...rest } = plain(p);
    return json(rest);
  });

  server.delete('/admin-network-policy/:name', function ({ db }, request) {
    const policies = collection(db, 'norcaAdminNetworkPolicies');
    const p = policies.findBy({ Name: request.params.name });
    if (!p) return error(404, 'admin network policy not found');
    policies.remove(p.id);
    return json(null);
  });
}

function instance(name, i, extra = {}) {
  return {
    ID: `_nomad-task-${name}-${i}`,
    ServiceName: name,
    Namespace: 'default',
    NodeID: `node-${i}`,
    Datacenter: 'dc1',
    JobID: name,
    AllocID: `${name}-alloc-${i}-0000-0000-000000000000`,
    Tags: ['http'],
    Address: `10.20.4.${10 + i}`,
    Port: 8080,
    Ready: true,
    Health: 'passing',
    Checks: [
      { Name: 'startup', Role: 'startup', Status: 'passing', Output: '' },
      { Name: 'ready', Role: 'readiness', Status: 'passing', Output: '' },
      {
        Name: 'alive',
        Role: 'liveness',
        Status: 'passing',
        Output: 'HTTP 200',
      },
    ],
    HealthUpdatedAt: Date.now() * 1e6,
    Meta: { 'norca-mtls': 'app' },
    Discovery: null,
    External: null,
    CreateIndex: 10,
    ModifyIndex: 10,
    ...extra,
  };
}

/**
 * Seeds services (api with one not-ready instance, postgres, an external
 * service), intentions, a network policy and an admin network policy.
 */
export function seedNetwork(server) {
  const db = server.db;
  const services = collection(db, 'norcaServices');
  services.insert(instance('api', 1));
  services.insert(instance('api', 2));
  services.insert(
    instance('api', 3, {
      Ready: false,
      Health: 'warning',
      Checks: [
        {
          Name: 'ready',
          Role: 'readiness',
          Status: 'warning',
          Output: 'HTTP 503',
        },
      ],
    })
  );
  services.insert(
    instance('postgres', 1, {
      Tags: ['primary'],
      Port: 5432,
      Meta: { 'norca-mtls': 'sidecar' },
    })
  );
  services.insert(
    instance('billing-saas', 1, {
      JobID: '',
      AllocID: '',
      NodeID: '',
      Tags: ['external'],
      Meta: null,
      Address: 'billing.example.com',
      Port: 443,
      External: { Kind: 'address', Source: 'api', LocalID: '', Checks: null },
    })
  );
  collection(db, 'norcaMtlsRoots').insert({
    ID: 'root-1',
    Region: 'global',
    TrustDomain: 'norca.local',
    RootCert: '',
    Active: true,
    NotBefore: new Date(Date.now() - 86400000).toISOString(),
    NotAfter: new Date(Date.now() + 300 * 86400000).toISOString(),
    RotatedOutAt: '0001-01-01T00:00:00Z',
  });
  const intentions = collection(db, 'norcaIntentions');
  intentions.insert({
    Namespace: 'default',
    Destination: 'postgres',
    SourceNamespace: 'default',
    Source: 'api',
    Action: 'allow',
    Description: 'api reads and writes the database',
    CreateIndex: 20,
    ModifyIndex: 20,
  });
  intentions.insert({
    Namespace: 'default',
    Destination: 'postgres',
    SourceNamespace: 'default',
    Source: 'worker',
    Action: 'deny',
    Description: 'the worker goes through the api',
    CreateIndex: 21,
    ModifyIndex: 21,
  });
  intentions.insert({
    Namespace: 'default',
    Destination: 'api',
    SourceNamespace: 'default',
    Source: '*',
    Action: 'allow',
    Description: '',
    CreateIndex: 22,
    ModifyIndex: 22,
  });
  collection(db, 'norcaNetworkPolicies').insert({
    Name: 'db-ingress',
    Namespace: 'default',
    Description: 'Only the api reaches the database',
    Selector: { app: 'postgres' },
    Ingress: [
      {
        Name: 'from-api',
        Action: 'allow',
        Peers: [{ CIDR: null, Namespace: 'default', Selector: { app: 'api' } }],
        Ports: ['5432/tcp'],
      },
    ],
    Egress: null,
    DefaultDeny: ['ingress'],
    Enforcement: 'required',
    CreateIndex: 30,
    ModifyIndex: 30,
    Status: [
      {
        ID: 'pg-alloc-1-0000-0000-000000000000',
        Namespace: 'default',
        JobID: 'postgres',
        TaskGroup: 'db',
        NodeID: 'node-1',
        NodeName: 'client-1',
        NetworkMode: 'bridge',
        Enforceable: true,
        Reason: '',
      },
      {
        ID: 'pg-alloc-2-0000-0000-000000000000',
        Namespace: 'default',
        JobID: 'postgres',
        TaskGroup: 'db',
        NodeID: 'node-2',
        NodeName: 'client-2',
        NetworkMode: 'host',
        Enforceable: false,
        Reason: 'host network mode cannot be filtered',
      },
    ],
  });
  collection(db, 'norcaAdminNetworkPolicies').insert({
    Name: 'block-metadata',
    Description: 'No workload reaches the cloud metadata service',
    Priority: 10,
    Namespaces: ['*'],
    Selector: null,
    Ingress: null,
    Egress: [
      {
        Name: 'metadata',
        Action: 'deny',
        Peers: [
          { CIDR: ['169.254.169.254/32'], Namespace: '', Selector: null },
        ],
        Ports: null,
      },
    ],
    Enforcement: 'best_effort',
    CreateIndex: 40,
    ModifyIndex: 40,
    Status: [],
  });
}
