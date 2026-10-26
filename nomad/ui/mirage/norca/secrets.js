/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, plain, error, json, inNamespace, body } from './util';

// Mirage routes of the built-in secret store (norcaSecrets: the Secret
// metadata JSON of the API plus Values, the items per version, which only
// the value read returns) and of the seal status (norcaSeal: one
// SealStatusResponse).

function nextIndex(db) {
  const meta = collection(db, 'norcaSecretIndex');
  const row = meta.find(1) || meta.insert({ id: 1, index: 2000 });
  meta.update(1, { index: row.index + 1 });
  return row.index + 1;
}

function hashOf(items) {
  // a stand-in for the keyed hash: stable, not the value
  const text = JSON.stringify(
    Object.keys(items)
      .sort()
      .map((k) => [k, items[k]])
  );
  let h = 0;
  for (const ch of text) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return `hmac-sha256:${h.toString(16).padStart(8, '0')}${'ab'.repeat(12)}`;
}

function secretPath(request) {
  return decodeURIComponent(request.params.path || '');
}

function find(db, namespace, path) {
  return collection(db, 'norcaSecrets').where(
    (s) => (s.Namespace || 'default') === namespace && s.Path === path
  )[0];
}

// The API form: metadata only, never values.
function view(s) {
  // eslint-disable-next-line no-unused-vars
  const { Values, ...rest } = plain(s);
  return rest;
}

function stub(s) {
  const v = view(s);
  return {
    Namespace: v.Namespace,
    Path: v.Path,
    Description: v.Description,
    Owner: v.Owner,
    Labels: v.Labels,
    ExpiresAt: v.ExpiresAt,
    RotateEvery: v.RotateEvery,
    CurrentVersion: v.CurrentVersion,
    Deleted: v.Deleted,
    Destroyed: v.Destroyed,
    ValueHash: v.ValueHash,
    CreateIndex: v.CreateIndex,
    ModifyIndex: v.ModifyIndex,
    CreateTime: v.CreateTime,
    ModifyTime: v.ModifyTime,
  };
}

function current(secret) {
  const v = (secret.Versions || []).find(
    (x) => x.Version === secret.CurrentVersion
  );
  return {
    Deleted: !!v?.Deleted,
    Destroyed: !!v?.Destroyed,
    ValueHash: v && !v.Destroyed ? v.ValueHash : '',
  };
}

function checkIndex(queryParams, secret) {
  if (queryParams.cas === undefined) return null;
  const cas = Number(queryParams.cas);
  if (cas === 0 ? !!secret : cas !== secret?.ModifyIndex)
    return error(
      409,
      'secret was modified concurrently (check index mismatch)'
    );
  return null;
}

export default function registerSecretRoutes(server) {
  server.get('/secrets', function ({ db }, { queryParams }) {
    const namespace = queryParams.namespace || 'default';
    const prefix = queryParams.prefix || '';
    return json(
      collection(db, 'norcaSecrets')
        .where(
          (s) => inNamespace(s, namespace) && String(s.Path).startsWith(prefix)
        )
        .map(stub)
    );
  });

  server.get('/secret/*path', function ({ db }, request) {
    const { queryParams } = request;
    const namespace = queryParams.namespace || 'default';
    const s = find(db, namespace, secretPath(request));
    if (!s) return error(404, 'secret not found');
    if (queryParams.metadata !== undefined) return json(view(s), s.ModifyIndex);
    const version = Number(queryParams.version) || s.CurrentVersion;
    const v = (s.Versions || []).find((x) => x.Version === version);
    if (!v) return error(404, `secret version ${version} not found`);
    if (v.Destroyed) return error(404, `version ${version} was destroyed`);
    if (v.Deleted)
      return error(404, `version ${version} is deleted; undelete it first`);
    return json(
      { Secret: view(s), Version: version, Items: s.Values[version] || {} },
      s.ModifyIndex
    );
  });

  server.put('/secret/*path', function ({ db }, request) {
    const { queryParams } = request;
    const namespace = queryParams.namespace || 'default';
    const path = secretPath(request);
    const { Items, Meta } = body(request);
    if (!Items && !Meta)
      return error(400, 'secret write requires Items or Meta');
    const stored = find(db, namespace, path);
    const conflict = checkIndex(queryParams, stored);
    if (conflict) return conflict;
    if (!stored && !Items) return error(404, 'secret not found');
    const index = nextIndex(db);
    const now = Date.now() * 1e6;
    const meta = Meta
      ? {
          Description: Meta.Description || '',
          Owner: Meta.Owner || '',
          Labels: Meta.Labels || null,
          ExpiresAt: Meta.ExpiresAt || null,
          RotateEvery: Meta.RotateEvery || 0,
          MaxVersions: Meta.MaxVersions || 10,
        }
      : {};
    const secret = stored
      ? { ...view(stored), Values: { ...stored.Values } }
      : {
          Namespace: namespace,
          Path: path,
          Description: '',
          Owner: '',
          Labels: null,
          ExpiresAt: null,
          RotateEvery: 0,
          MaxVersions: 10,
          CurrentVersion: 0,
          Versions: [],
          Values: {},
          CreateIndex: index,
          CreateTime: now,
        };
    Object.assign(secret, meta);
    if (Items) {
      const version = (secret.CurrentVersion || 0) + 1;
      secret.Versions = [
        ...(secret.Versions || []),
        {
          Version: version,
          CreateTime: now,
          Deleted: false,
          Destroyed: false,
          ValueHash: hashOf(Items),
        },
      ].slice(-(secret.MaxVersions || 10));
      secret.Values[version] = { ...Items };
      secret.CurrentVersion = version;
      secret.ItemKeys = Object.keys(Items).sort();
    }
    Object.assign(secret, current(secret), {
      ModifyIndex: index,
      ModifyTime: now,
    });
    const c = collection(db, 'norcaSecrets');
    if (stored) c.update(stored.id, secret);
    else c.insert(secret);
    return json({ Secret: view(secret) }, index);
  });

  function versionsOp(op) {
    return function ({ db }, request) {
      const { queryParams } = request;
      const namespace = queryParams.namespace || 'default';
      let path = secretPath(request);
      if (op !== 'delete') path = path.replace(new RegExp(`/${op}$`), '');
      const stored = find(db, namespace, path);
      if (!stored) return error(404, 'secret not found');
      const conflict = checkIndex(queryParams, stored);
      if (conflict) return conflict;
      const versions = queryParams.versions
        ? String(queryParams.versions).split(',').map(Number)
        : op === 'destroy'
        ? null
        : [stored.CurrentVersion];
      const c = collection(db, 'norcaSecrets');
      if (!versions) {
        c.remove(stored.id);
        return json({ Secret: null }, nextIndex(db));
      }
      const secret = { ...stored, Values: { ...stored.Values } };
      secret.Versions = (stored.Versions || []).map((v) => {
        if (!versions.includes(v.Version)) return v;
        if (op === 'delete') return { ...v, Deleted: true };
        if (op === 'undelete') return { ...v, Deleted: false };
        delete secret.Values[v.Version];
        return { ...v, Destroyed: true, ValueHash: '' };
      });
      const index = nextIndex(db);
      Object.assign(secret, current(secret), {
        ModifyIndex: index,
        ModifyTime: Date.now() * 1e6,
      });
      c.update(stored.id, secret);
      return json({ Secret: view(secret) }, index);
    };
  }

  server.delete('/secret/*path', versionsOp('delete'));
  server.post('/secret/*path', function (schema, request) {
    const path = secretPath(request);
    if (path.endsWith('/undelete'))
      return versionsOp('undelete').call(this, schema, request);
    if (path.endsWith('/destroy'))
      return versionsOp('destroy').call(this, schema, request);
    return error(
      400,
      'POST is only valid for /undelete and /destroy; write with PUT'
    );
  });

  server.get('/operator/seal', function ({ db }) {
    const row = collection(db, 'norcaSeal').find(1);
    if (!row) return json({ Config: null, Servers: [] });
    return json(plain(row));
  });
}

// A secret with its versions; values per version.
export function secretRecord({
  namespace = 'default',
  path,
  description = '',
  owner = '',
  labels = null,
  values,
  deleted = [],
  destroyed = [],
  expiresAt = null,
  rotateEvery = 0,
  index = 100,
}) {
  const now = Date.now() * 1e6;
  const versions = values.map((items, i) => ({
    Version: i + 1,
    CreateTime: now - (values.length - i) * 86400e9,
    Deleted: deleted.includes(i + 1),
    Destroyed: destroyed.includes(i + 1),
    ValueHash: destroyed.includes(i + 1) ? '' : hashOf(items),
  }));
  const record = {
    Namespace: namespace,
    Path: path,
    Description: description,
    Owner: owner,
    Labels: labels,
    ExpiresAt: expiresAt,
    RotateEvery: rotateEvery,
    MaxVersions: 10,
    CurrentVersion: values.length,
    ItemKeys: Object.keys(values[values.length - 1]).sort(),
    Versions: versions,
    Values: Object.fromEntries(
      values.map((items, i) => [
        i + 1,
        destroyed.includes(i + 1) ? null : items,
      ])
    ),
    CreateIndex: index,
    ModifyIndex: index + values.length,
    CreateTime: versions[0].CreateTime,
    ModifyTime: versions[versions.length - 1].CreateTime,
  };
  return Object.assign(record, current(record));
}

export function seedSecrets(server) {
  const secrets = collection(server.db, 'norcaSecrets');
  secrets.insert(
    secretRecord({
      path: 'prod/db/postgres',
      description: 'Credentials of the billing database',
      owner: 'team-billing',
      labels: { team: 'billing', tier: 'db' },
      values: [
        { username: 'billing', password: 'first-canary-value' },
        { username: 'billing', password: 'second-canary-value' },
        { username: 'billing', password: 'third-canary-value' },
      ],
      destroyed: [1],
      rotateEvery: 30 * 86400e9,
    })
  );
  secrets.insert(
    secretRecord({
      path: 'prod/api/stripe',
      description: 'Payment provider API key',
      owner: 'team-payments',
      values: [{ api_key: 'sk-canary-value' }],
      index: 120,
    })
  );
  secrets.insert(
    secretRecord({
      path: 'old/token',
      values: [{ token: 'old-canary-value' }],
      deleted: [1],
      index: 140,
    })
  );
  secrets.insert(
    secretRecord({
      namespace: 'staging',
      path: 'staging/db/postgres',
      values: [{ username: 'test', password: 'staging-canary-value' }],
      index: 160,
    })
  );
  collection(server.db, 'norcaSeal').insert({
    id: 1,
    Config: {
      Seals: [{ Type: 'shares', KeyID: 'k-1', Threshold: 2, Shares: 3 }],
      Rewrap: null,
    },
    Servers: [
      {
        ID: 's1',
        Name: 'server-1.global',
        Address: '10.0.0.1:4647',
        Leader: true,
        Sealed: false,
        Initialized: true,
        Seals: ['shares'],
        ConfiguredSeal: 'shares',
        Threshold: 2,
        Shares: 3,
        Progress: 0,
        SecretCoreInitialized: true,
        SecretCoreSealed: false,
        SecretCoreActive: true,
      },
      {
        ID: 's2',
        Name: 'server-2.global',
        Address: '10.0.0.2:4647',
        Leader: false,
        Sealed: true,
        Initialized: true,
        Seals: ['shares'],
        ConfiguredSeal: 'shares',
        Threshold: 2,
        Shares: 3,
        Progress: 1,
        SecretCoreInitialized: true,
        SecretCoreSealed: true,
        SecretCoreActive: false,
      },
    ],
  });
}
