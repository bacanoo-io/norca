/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, body, plain, error, json, inNamespace } from './util';

// Mirage routes of the backup API (/v1/operator/backup*). Records are the
// API's BackupRecord JSON plus `Contents`: the browsable content of the
// backup by type (Jobs, JobVersions, Variables, ...), which the server would
// decrypt from the store. `norcaBackupCurrent` holds the same for the
// current state (comparison and restore previews).

const TYPES = {
  namespaces: 'Namespaces',
  jobs: 'Jobs',
  'job-versions': 'JobVersions',
  variables: 'Variables',
  'acl-policies': 'ACLPolicies',
  'acl-roles': 'ACLRoles',
  deployments: 'Deployments',
  allocations: 'Allocations',
};

const ZERO_TIME = '0001-01-01T00:00:00Z';

export const backupIdentity = 'AGE-SECRET-KEY-1MIRAGEMIRAGEMIRAGE';

function tokenIdentity(schema, request) {
  const secret = request.requestHeaders['X-Nomad-Token'];
  const token = secret && schema.tokens?.findBy({ secretId: secret });
  return token ? `token:${token.accessorId}` : 'token:anonymous';
}

function objectId(type, item) {
  if (type === 'variables') return item.Path;
  if (type === 'namespaces' || type === 'acl-policies' || type === 'acl-roles')
    return item.Name;
  if (type === 'job-versions') return `${item.ID}@${item.Version}`;
  return item.ID;
}

function contentsOf(record, type) {
  return (record?.Contents || {})[TYPES[type]] || [];
}

function filterItems(items, type, { job, prefix, namespace }) {
  return items.filter((item) => {
    if (
      type !== 'acl-policies' &&
      type !== 'acl-roles' &&
      type !== 'namespaces'
    )
      if (!inNamespace(item, namespace)) return false;
    if (job && type !== 'jobs' && (item.JobID || item.ID) !== job) return false;
    if (job && type === 'jobs' && item.ID !== job) return false;
    if (prefix && !String(objectId(type, item)).startsWith(prefix))
      return false;
    return true;
  });
}

export function diffContents(from, to, types, filters) {
  const objects = [];
  for (const type of types) {
    const a = filterItems(contentsOf(from, type), type, filters);
    const b = filterItems(contentsOf(to, type), type, filters);
    const key = (item) => `${item.Namespace || ''}/${objectId(type, item)}`;
    const bm = new Map(b.map((i) => [key(i), i]));
    const am = new Map(a.map((i) => [key(i), i]));
    for (const [k, item] of am) {
      const other = bm.get(k);
      const base = {
        Type: type,
        Namespace: item.Namespace || '',
        ID: objectId(type, item),
      };
      if (!other) {
        objects.push({
          ...base,
          Change: 'deleted',
          FromVersion: item.Version ?? null,
          ToVersion: null,
          FromStatus: item.Status || '',
          ToStatus: '',
        });
      } else if (
        (item.ModifyIndex || item.JobModifyIndex) !==
          (other.ModifyIndex || other.JobModifyIndex) ||
        item.Status !== other.Status
      ) {
        objects.push({
          ...base,
          Change: 'modified',
          FromVersion: item.Version ?? null,
          ToVersion: other.Version ?? null,
          FromStatus: item.Status || '',
          ToStatus: other.Status || '',
        });
      }
    }
    for (const [k, item] of bm) {
      if (!am.has(k))
        objects.push({
          Type: type,
          Change: 'added',
          Namespace: item.Namespace || '',
          ID: objectId(type, item),
          FromVersion: null,
          ToVersion: item.Version ?? null,
          FromStatus: '',
          ToStatus: item.Status || '',
        });
    }
  }
  return objects;
}

function record(db, id) {
  return collection(db, 'norcaBackups').findBy({ ID: id });
}

function publicRecord(r) {
  if (!r) return r;
  // eslint-disable-next-line no-unused-vars
  const { Contents, ...rest } = plain(r);
  return rest;
}

function checkIdentity(payload, r) {
  if (!payload.AgeIdentity)
    return error(400, `an age identity is required to decrypt backup ${r.ID}`);
  if (payload.AgeIdentity.trim() !== backupIdentity)
    return error(400, 'no identity matched any of the recipients');
  if (r.Status === 'failed')
    return error(400, `backup ${r.ID} has status "failed"`);
  return null;
}

function newId(index) {
  const d = new Date();
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}${p(d.getUTCMonth() + 1)}${p(
    d.getUTCDate()
  )}T${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(
    d.getUTCSeconds()
  )}Z-${index}`;
}

function regionName(request) {
  return request.queryParams.region || 'global';
}

export default function registerBackupRoutes(server) {
  server.get('/operator/backups', function ({ db }, request) {
    const region = request.queryParams.region;
    const all = collection(db, 'norcaBackups')
      .where((r) => !region || !r.Region || r.Region === region)
      .map(publicRecord)
      .sort((a, b) => (a.ID < b.ID ? 1 : -1));
    const schedules = collection(db, 'norcaBackupSchedules');
    const schedule = plain(
      schedules.findBy({ Region: region || 'global' }) || schedules[0]
    ) || {
      Enabled: false,
      Interval: 0,
      Schedule: '',
      Destinations: [],
      LastSuccess: ZERO_TIME,
      LastSuccessID: '',
      LastAttempt: ZERO_TIME,
      LastError: '',
      NextDue: ZERO_TIME,
      Overdue: false,
    };
    // eslint-disable-next-line no-unused-vars
    const { Region, ...status } = schedule;
    return json({ Backups: all, Schedule: status });
  });

  const save = function ({ db }, request) {
    const payload = body(request);
    const backups = collection(db, 'norcaBackups');
    if (db.norcaBackupConfig?.[0]?.noDestination)
      return error(500, 'no backup destination is configured');
    const now = new Date().toISOString();
    const id = newId(1000 + backups.length);
    const template = backups.where((r) => r.Status === 'complete')[0];
    const created = backups.insert({
      ...(template ? plain(template) : {}),
      ID: id,
      Region: regionName(request),
      Trigger: 'manual',
      Status: 'complete',
      StatusDescription: '',
      StartedAt: now,
      CompletedAt: now,
      Labels: payload.Labels || {},
      Pinned: !!payload.Pin,
      Destinations: template?.Destinations || [],
      Summary: template?.Summary || {},
      Contents: template?.Contents || {},
    });
    return json({
      ID: id,
      Backup: payload.Detach ? null : publicRecord(created),
    });
  };
  server.post('/operator/backups', save);
  server.put('/operator/backups', save);

  server.get('/operator/backup/restores', function ({ db }) {
    return json(
      collection(db, 'norcaBackupRestores')
        .map((r) => ({
          ...plain(r),
          Preview: r.Preview ? { ...r.Preview, Objects: null } : null,
        }))
        .sort((a, b) => (a.CreateTime < b.CreateTime ? 1 : -1))
    );
  });

  server.get('/operator/backup/restore/:rid', function ({ db }, { params }) {
    const r = collection(db, 'norcaBackupRestores').findBy({ ID: params.rid });
    if (!r) return error(404, 'restore not found');
    return json(plain(r));
  });

  const approve = function (schema, request) {
    const restores = collection(schema.db, 'norcaBackupRestores');
    const r = restores.findBy({ ID: request.params.rid });
    if (!r) return error(404, `restore "${request.params.rid}" not found`);
    if (r.Status !== 'pending')
      return error(400, `full restore ${r.ID} is ${r.Status}`);
    const who = tokenIdentity(schema, request);
    if (who === r.RequestedBy)
      return error(
        403,
        "the requester of a full restore can't approve it; a second administrator must"
      );
    const payload = body(request);
    if ((payload.AgeIdentity || '').trim() !== backupIdentity)
      return error(400, 'no identity matched any of the recipients');
    const now = new Date().toISOString();
    const updated = restores.update(r.id, {
      Status: 'complete',
      Approvals: [who],
      ApprovedAt: now,
      ModifyTime: now,
      PreRestoreBackupID: newId(9000),
      StatusDescription: `restored backup ${
        r.BackupID
      }; undo with backup ${newId(9000)}`,
    });
    return json(plain(updated));
  };
  server.post('/operator/backup/restore/:rid/approve', approve);
  server.put('/operator/backup/restore/:rid/approve', approve);

  const reject = function ({ db }, request) {
    const restores = collection(db, 'norcaBackupRestores');
    const r = restores.findBy({ ID: request.params.rid });
    if (!r) return error(404, `restore "${request.params.rid}" not found`);
    const reason = body(request).Reason;
    const updated = restores.update(r.id, {
      Status: 'rejected',
      StatusDescription: reason ? `rejected: ${reason}` : 'rejected',
      ModifyTime: new Date().toISOString(),
    });
    return json(plain(updated));
  };
  server.post('/operator/backup/restore/:rid/reject', reject);
  server.put('/operator/backup/restore/:rid/reject', reject);

  server.get('/operator/backup/:id', function ({ db }, { params }) {
    const r = record(db, params.id);
    if (!r) return error(404, 'backup not found');
    return json(publicRecord(r));
  });

  const update = function ({ db }, request) {
    const r = record(db, request.params.id);
    if (!r) return error(404, `backup "${request.params.id}" not found`);
    const payload = body(request);
    const changes = {};
    if (payload.Pinned !== undefined && payload.Pinned !== null)
      changes.Pinned = payload.Pinned;
    let labels = { ...(r.Labels || {}) };
    if (payload.Labels) labels = { ...payload.Labels };
    (payload.RemoveLabels || []).forEach((k) => delete labels[k]);
    changes.Labels = labels;
    const updated = collection(db, 'norcaBackups').update(r.id, changes);
    return json(publicRecord(updated));
  };
  server.put('/operator/backup/:id', update);
  server.post('/operator/backup/:id', update);

  server.delete('/operator/backup/:id', function ({ db }, { params }) {
    const r = record(db, params.id);
    if (!r) return error(404, `backup "${params.id}" not found`);
    if (r.Pinned) return error(400, `backup ${r.ID} is pinned; unpin it first`);
    collection(db, 'norcaBackups').remove(r.id);
    return json(null);
  });

  server.get(
    '/operator/backup/:id/download-url',
    function ({ db }, { params }) {
      const r = record(db, params.id);
      if (!r) return error(404, 'backup not found');
      return json({
        URL: `https://s3.example.com/${
          r.Path || r.ID
        }/state.bin?X-Amz-Signature=mirage`,
        ExpiresAt: new Date(Date.now() + 15 * 60000).toISOString(),
      });
    }
  );

  const contents = function ({ db }, request) {
    const r = record(db, request.params.id);
    if (!r) return error(404, 'backup not found');
    const denied = checkIdentity(body(request), r);
    if (denied) return denied;
    const type = request.queryParams.type || 'jobs';
    if (!TYPES[type])
      return error(400, `unknown content type "${type}", must be one of ...`);
    const { job, prefix } = request.queryParams;
    const namespace = request.queryParams.namespace || 'default';
    if (type === 'job-versions' && !job)
      return error(400, 'the job-versions content type requires a job ID');
    const result = {
      BackupID: r.ID,
      RaftIndex: r.RaftIndex,
      Type: type,
    };
    Object.values(TYPES).forEach((key) => (result[key] = null));
    result[TYPES[type]] = filterItems(contentsOf(r, type), type, {
      job,
      prefix,
      namespace,
    });
    return json(result);
  };
  server.post('/operator/backup/:id/contents', contents);
  server.put('/operator/backup/:id/contents', contents);

  const diff = function ({ db }, request) {
    const r = record(db, request.params.id);
    if (!r) return error(404, 'backup not found');
    const denied = checkIdentity(body(request), r);
    if (denied) return denied;
    const against = request.queryParams.against || 'current';
    const other =
      against === 'current'
        ? collection(db, 'norcaBackupCurrent')[0]
        : record(db, against);
    if (!other) return error(404, `backup ${against} not found`);
    const type = request.queryParams.type;
    const types = type
      ? [type]
      : Object.keys(TYPES).filter((t) => t !== 'job-versions');
    const { job } = request.queryParams;
    const namespace = request.queryParams.namespace || 'default';
    const objects = diffContents(r, other, types, { job, namespace });
    let jobDiff = null;
    if (job && objects.some((o) => o.Type === 'jobs' && o.ID === job)) {
      jobDiff = (r.JobDiffs || {})[job] || {
        Type: 'Edited',
        ID: job,
        Fields: [
          {
            Type: 'Edited',
            Name: 'Meta[version]',
            Old: 'from backup',
            New: 'current',
            Annotations: null,
          },
        ],
        Objects: null,
        TaskGroups: null,
      };
    }
    return json({
      From: r.ID,
      To: against,
      FromIndex: r.RaftIndex,
      ToIndex: other.RaftIndex || 0,
      Objects: objects,
      JobDiff: jobDiff,
    });
  };
  server.post('/operator/backup/:id/diff', diff);
  server.put('/operator/backup/:id/diff', diff);

  const restore = function (schema, request) {
    const { db } = schema;
    const r = record(db, request.params.id);
    if (!r) return error(404, 'backup not found');
    const payload = body(request);
    const denied = checkIdentity(payload, r);
    if (denied) return denied;
    const mode = payload.Mode || 'selective';
    const restores = collection(db, 'norcaBackupRestores');
    const now = new Date().toISOString();
    const current = collection(db, 'norcaBackupCurrent')[0];

    if (mode === 'full') {
      if ((payload.Items || []).length)
        return error(400, 'a full restore takes no items');
      const objects = diffContents(
        r,
        current,
        Object.keys(TYPES).filter((t) => t !== 'job-versions'),
        { namespace: '*' }
      ).map((o) => ({
        ...o,
        Change:
          o.Change === 'added'
            ? 'removed'
            : o.Change === 'deleted'
            ? 'restored'
            : 'reverted',
      }));
      const counts = {};
      objects.forEach((o) => {
        const key = `${o.Type}/${o.Change}`;
        counts[key] = (counts[key] || 0) + 1;
      });
      const preview = {
        BackupID: r.ID,
        Region: r.Region || regionName(request),
        BackupIndex: r.RaftIndex,
        CurrentIndex: current?.RaftIndex || 0,
        Counts: counts,
        AllocationsStopped: r.PreviewAllocationsStopped ?? 4,
        ACLTokensRemoved: 0,
        KeysAdded: [],
        Warnings: [
          'every change since the backup is lost; an automatic backup of the current state is taken first',
        ],
        Objects: objects,
        Truncated: false,
      };
      if (payload.DryRun)
        return json({ Restore: null, Results: null, Preview: preview });
      const region = r.Region || regionName(request);
      if (payload.Confirm !== region)
        return error(
          400,
          `confirm a full restore with the name of the region ("${region}")`
        );
      const pending = restores.findBy({ Mode: 'full', Status: 'pending' });
      if (pending)
        return error(
          409,
          `full restore ${pending.ID} is waiting for approval; reject it first`
        );
      const created = restores.insert({
        ID: `restore-${restores.length + 1}-0000-0000-000000000000`,
        BackupID: r.ID,
        Mode: 'full',
        Items: [],
        Status: 'pending',
        StatusDescription: 'waiting for approval',
        Results: null,
        RequestedBy: tokenIdentity(schema, request),
        RequestedByName: 'Mirage',
        Approvals: null,
        ApprovedAt: ZERO_TIME,
        RejectedBy: '',
        Destination: payload.Destination || '',
        ExpiresAt: new Date(Date.now() + 3600000).toISOString(),
        PreRestoreBackupID: '',
        Preview: preview,
        CreateTime: now,
        ModifyTime: now,
      });
      return json({ Restore: plain(created), Results: null, Preview: preview });
    }

    const items = payload.Items || [];
    if (!items.length)
      return error(400, 'must specify at least one item to restore');
    const results = items.map((item, i) => {
      const type = {
        job: 'jobs',
        variable: 'variables',
        namespace: 'namespaces',
        'acl-policy': 'acl-policies',
        'acl-role': 'acl-roles',
      }[item.Type];
      if (!type)
        return {
          Item: item,
          Action: 'none',
          Error: `invalid restore item type "${item.Type}"`,
        };
      const ns = item.Namespace || 'default';
      const inBackup = contentsOf(r, type).find(
        (o) =>
          objectId(type, o) === item.ID &&
          (['namespaces', 'acl-policies', 'acl-roles'].includes(type) ||
            (o.Namespace || 'default') === ns)
      );
      if (!inBackup)
        return {
          Item: item,
          Action: 'none',
          Description: '',
          CurrentIndex: 0,
          Index: 0,
          Error: `${item.Type} "${item.ID}" not found in the backup`,
        };
      const now = contentsOf(current, type).find(
        (o) => objectId(type, o) === item.ID
      );
      let description = `restore ${item.Type} "${item.ID}"`;
      if (type === 'jobs')
        description = `register version ${
          item.Version ?? inBackup.Version
        } of the backup as a new version of job "${item.ID}"${
          now ? ` (current version ${now.Version})` : ''
        }`;
      let err = '';
      if (
        type === 'variables' &&
        now &&
        !payload.DryRun &&
        item.CheckIndex !== now.ModifyIndex
      )
        err = `variable "${item.ID}" exists (modify index ${now.ModifyIndex}); give that check index to overwrite it`;
      if (type === 'variables' && now && payload.DryRun && !item.CheckIndex)
        description += `; it exists (modify index ${now.ModifyIndex}), give that check index to overwrite it`;
      return {
        Item: item,
        Action: now ? 'update' : 'create',
        Description: description,
        CurrentIndex: now?.ModifyIndex || now?.JobModifyIndex || 0,
        Index: payload.DryRun || err ? 0 : 5000 + i,
        Error: err,
      };
    });
    if (payload.DryRun)
      return json({ Restore: null, Results: results, Preview: null });
    const failed = results.filter((x) => x.Error).length;
    const created = restores.insert({
      ID: `restore-${restores.length + 1}-1111-1111-111111111111`,
      BackupID: r.ID,
      Mode: 'selective',
      Items: items,
      Status: failed ? 'failed' : 'complete',
      StatusDescription: failed
        ? `${failed} of ${items.length} items failed`
        : '',
      Results: results,
      RequestedBy: tokenIdentity(schema, request),
      RequestedByName: '',
      Approvals: null,
      ApprovedAt: ZERO_TIME,
      RejectedBy: '',
      Destination: '',
      ExpiresAt: ZERO_TIME,
      PreRestoreBackupID: '',
      Preview: null,
      CreateTime: now,
      ModifyTime: now,
    });
    return json({ Restore: plain(created), Results: results, Preview: null });
  };
  server.post('/operator/backup/:id/restore', restore);
  server.put('/operator/backup/:id/restore', restore);
}
