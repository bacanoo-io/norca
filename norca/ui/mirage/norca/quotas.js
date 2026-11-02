/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, body, plain, error, json } from './util';

// Mirage routes of the quota API: specs in norcaQuotas, usages in
// norcaQuotaUsages ({ Name, Used: { <limit hash>: QuotaLimit } }).

function withHashes(spec) {
  return {
    ...spec,
    Enforce: spec.Enforce || 'schedule',
    Limits: (spec.Limits || []).map((l) => ({
      ...l,
      RegionLimit: l.RegionLimit || {},
      Hash: l.Hash || `hash-${spec.Name}-${l.Region}`,
    })),
  };
}

export default function registerQuotaRoutes(server) {
  server.get('/quotas', function ({ db }, { queryParams }) {
    const prefix = queryParams.prefix || '';
    return json(
      collection(db, 'norcaQuotas')
        .where((q) => q.Name.startsWith(prefix))
        .map(plain)
    );
  });

  server.get('/quota-usages', function ({ db }, { queryParams }) {
    const region = queryParams.region;
    return json(
      collection(db, 'norcaQuotaUsages').map((u) => {
        const used = {};
        Object.entries(u.Used || {}).forEach(([hash, limit]) => {
          if (!region || !limit.Region || limit.Region === region)
            used[hash] = limit;
        });
        return { ...plain(u), Used: used };
      })
    );
  });

  server.get('/quota/usage/:name', function ({ db }, { params }) {
    const usage = collection(db, 'norcaQuotaUsages').findBy({
      Name: params.name,
    });
    if (!usage) return error(404, 'Quota usage not found');
    return json(plain(usage));
  });

  server.get('/quota/:name', function ({ db }, { params }) {
    const spec = collection(db, 'norcaQuotas').findBy({ Name: params.name });
    if (!spec) return error(404, 'Quota not found');
    return json(plain(spec));
  });

  const upsert = function ({ db }, request) {
    const spec = body(request);
    if (request.params.name && spec.Name !== request.params.name)
      return error(400, 'Quota name does not match request path');
    if (!/^[a-zA-Z0-9-_]{1,128}$/.test(spec.Name || ''))
      return error(
        400,
        `invalid quota specification "${spec.Name}": invalid name`
      );
    const quotas = collection(db, 'norcaQuotas');
    const existing = quotas.findBy({ Name: spec.Name });
    const record = withHashes(spec);
    if (existing) quotas.update(existing.id, record);
    else quotas.insert(record);
    return json(null);
  };
  server.put('/quota/:name', upsert);
  server.post('/quota/:name', upsert);
  server.put('/quota', upsert);

  server.delete('/quota/:name', function ({ db }, { params }) {
    const quotas = collection(db, 'norcaQuotas');
    const spec = quotas.findBy({ Name: params.name });
    if (!spec) return error(404, `quota "${params.name}" not found`);
    const user = db.namespaces?.where((n) => n.quota === params.name)[0];
    if (user)
      return error(
        400,
        `quota "${params.name}" is in use by namespace "${
          user.name || user.id
        }"`
      );
    quotas.remove(spec.id);
    return json(null);
  });
}

/**
 * Seeds two quotas: team-a nearly full on CPU, team-b with an evaluation
 * blocked by it.
 */
export function seedQuotas(server, { region = 'global' } = {}) {
  const quotas = collection(server.db, 'norcaQuotas');
  const usages = collection(server.db, 'norcaQuotaUsages');
  const a = withHashes({
    Name: 'team-a',
    Description: 'Team A production',
    Limits: [
      {
        Region: region,
        RegionLimit: { CPU: 40000, MemoryMB: 81920 },
        Allocations: 200,
        VariablesLimit: 0,
      },
    ],
    TaskMax: { CPU: 8000, MemoryMB: 16384 },
  });
  const b = withHashes({
    Name: 'team-b',
    Description: 'Reporting',
    Limits: [
      {
        Region: region,
        RegionLimit: { CPU: 20000, MemoryMB: 40960 },
        Allocations: 100,
      },
    ],
    Enforce: 'submit',
  });
  quotas.insert(a);
  quotas.insert(b);
  usages.insert({
    Name: 'team-a',
    Used: {
      [a.Limits[0].Hash]: {
        Region: region,
        RegionLimit: { CPU: 28400, MemoryMB: 51200 },
        Allocations: 118,
        VariablesLimit: 3,
      },
    },
  });
  usages.insert({
    Name: 'team-b',
    Used: {
      [b.Limits[0].Hash]: {
        Region: region,
        RegionLimit: { CPU: 19200, MemoryMB: 30100 },
        Allocations: 61,
      },
    },
  });
}
