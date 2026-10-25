/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';
import { formatBytes } from 'nomad-ui/utils/units';

// Requests and display helpers of the backup pages (backup-restore-plan.md,
// "Web UI"). The API is /v1/operator/backup*; content, comparison and
// restore requests carry the age identity in the body.

export const ZERO_TIME = '0001-01-01T00:00:00Z';

export const CONTENT_TYPES = [
  { id: 'jobs', label: 'Jobs', key: 'Jobs', restoreType: 'job' },
  { id: 'deployments', label: 'Deployments', key: 'Deployments' },
  { id: 'allocations', label: 'Allocations', key: 'Allocations' },
  {
    id: 'variables',
    label: 'Variables',
    key: 'Variables',
    restoreType: 'variable',
  },
  {
    id: 'namespaces',
    label: 'Namespaces',
    key: 'Namespaces',
    restoreType: 'namespace',
  },
  {
    id: 'acl-policies',
    label: 'ACL policies',
    key: 'ACLPolicies',
    restoreType: 'acl-policy',
  },
  {
    id: 'acl-roles',
    label: 'ACL roles',
    key: 'ACLRoles',
    restoreType: 'acl-role',
  },
];

export const RESTORE_TYPE_LABELS = {
  job: 'Job',
  variable: 'Variables path',
  namespace: 'Namespace',
  'acl-policy': 'ACL policy',
  'acl-role': 'ACL role',
};

export function isSet(time) {
  return !!time && time !== ZERO_TIME && !String(time).startsWith('0001-');
}

export function backupSize(record) {
  // the size of the state object in the first destination that has one
  for (const d of record?.Destinations || []) {
    const objects = Object.values(d.Objects || {});
    if (objects.length)
      return objects.reduce((sum, o) => sum + (o.Size || 0), 0);
  }
  return 0;
}

export function formatSize(bytes) {
  return bytes ? formatBytes(bytes) : '–';
}

export const TRIGGER_LABELS = {
  schedule: 'Scheduled',
  manual: 'Manual',
  'pre-restore': 'Before a restore',
};

export function statusTone(status) {
  return (
    {
      complete: 'success',
      verified: 'success',
      partial: 'warning',
      failed: 'danger',
      pending: 'warning',
      approved: 'info',
      running: 'info',
      rejected: 'neutral',
      expired: 'neutral',
    }[status] || 'neutral'
  );
}

export function formatInterval(ns) {
  if (!ns) return '';
  const minutes = Math.round(ns / 6e10);
  if (minutes % 1440 === 0) {
    const d = minutes / 1440;
    return d === 1 ? 'every day' : `every ${d} days`;
  }
  if (minutes % 60 === 0) {
    const h = minutes / 60;
    return h === 1 ? 'every hour' : `every ${h} hours`;
  }
  return `every ${minutes} minutes`;
}

export function scheduleLabel(schedule) {
  if (!schedule?.Enabled) return 'Not scheduled';
  if (schedule.Schedule) return `cron ${schedule.Schedule}`;
  return formatInterval(schedule.Interval) || 'Scheduled';
}

// Restore records name their requester as "token:<accessor>".
export function identityLabel(identity) {
  if (!identity) return '';
  const [kind, id] = String(identity).split(':');
  if (kind === 'token' && id) return `token ${id.split('-')[0]}`;
  return identity;
}

export function listBackups(token, region) {
  return apiRequest(token, '/v1/operator/backups', { params: { region } });
}

export function getBackup(token, region, id) {
  return apiRequest(token, `/v1/operator/backup/${encodeURIComponent(id)}`, {
    params: { region },
  });
}

export function updateBackup(token, region, id, data) {
  return apiRequest(token, `/v1/operator/backup/${encodeURIComponent(id)}`, {
    method: 'PUT',
    params: { region },
    data,
  });
}

export function deleteBackup(token, region, id) {
  return apiRequest(token, `/v1/operator/backup/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    params: { region },
  });
}

export function startBackup(token, region, { labels, pin }) {
  return apiRequest(token, '/v1/operator/backups', {
    method: 'POST',
    params: { region },
    data: { Labels: labels, Pin: !!pin, Detach: true },
  });
}

export function downloadURL(token, region, id) {
  return apiRequest(
    token,
    `/v1/operator/backup/${encodeURIComponent(id)}/download-url`,
    { params: { region } }
  );
}

export function backupContents(token, { region, id, identity, ...params }) {
  return apiRequest(
    token,
    `/v1/operator/backup/${encodeURIComponent(id)}/contents`,
    {
      method: 'POST',
      params: { region, ...params },
      data: { AgeIdentity: identity },
    }
  );
}

export function backupDiff(token, { region, id, identity, ...params }) {
  return apiRequest(
    token,
    `/v1/operator/backup/${encodeURIComponent(id)}/diff`,
    {
      method: 'POST',
      params: { region, ...params },
      data: { AgeIdentity: identity },
    }
  );
}

export function restoreBackup(token, { region, id, ...data }) {
  return apiRequest(
    token,
    `/v1/operator/backup/${encodeURIComponent(id)}/restore`,
    { method: 'POST', params: { region }, data }
  );
}

export function listRestores(token, region) {
  return apiRequest(token, '/v1/operator/backup/restores', {
    params: { region },
  });
}

export function getRestore(token, region, id) {
  return apiRequest(
    token,
    `/v1/operator/backup/restore/${encodeURIComponent(id)}`,
    { params: { region } }
  );
}

export function approveRestore(token, region, id, identity) {
  return apiRequest(
    token,
    `/v1/operator/backup/restore/${encodeURIComponent(id)}/approve`,
    { method: 'POST', params: { region }, data: { AgeIdentity: identity } }
  );
}

export function rejectRestore(token, region, id, reason) {
  return apiRequest(
    token,
    `/v1/operator/backup/restore/${encodeURIComponent(id)}/reject`,
    { method: 'POST', params: { region }, data: { Reason: reason || '' } }
  );
}

// The counts of a full restore preview ("jobs/removed": 3) as rows.
export function previewCounts(preview) {
  const byType = {};
  for (const [key, count] of Object.entries(preview?.Counts || {})) {
    const [type, change] = key.split('/');
    byType[type] = byType[type] || {
      type,
      removed: 0,
      restored: 0,
      reverted: 0,
    };
    byType[type][change] = count;
  }
  const order = CONTENT_TYPES.map((t) => t.id);
  return Object.values(byType)
    .map((row) => ({
      ...row,
      label: CONTENT_TYPES.find((t) => t.id === row.type)?.label || row.type,
    }))
    .sort((a, b) => order.indexOf(a.type) - order.indexOf(b.type));
}

// Flattens a Nomad JobDiff into rows: { path, type, old, new }.
export function flattenJobDiff(diff) {
  const rows = [];
  const walk = (node, path) => {
    if (!node) return;
    for (const f of node.Fields || []) {
      if (f.Type === 'None') continue;
      rows.push({
        path: [...path, f.Name].join(' › '),
        type: f.Type,
        old: f.Old,
        new: f.New,
      });
    }
    for (const o of node.Objects || []) {
      if (o.Type === 'None') continue;
      walk(o, [...path, o.Name]);
    }
    for (const g of node.TaskGroups || []) {
      if (g.Type === 'None') continue;
      walk(g, [...path, `group ${g.Name}`]);
      for (const t of g.Tasks || []) {
        if (t.Type === 'None') continue;
        walk(t, [...path, `group ${g.Name}`, `task ${t.Name}`]);
      }
    }
  };
  walk(diff, []);
  return rows;
}

// A restore item (BackupRestoreItem): its identity and its label.
export function itemKey(item) {
  return [item.Type, item.Namespace || '', item.ID, item.Version ?? ''].join(
    '|'
  );
}

export function itemLabel(item) {
  const type = RESTORE_TYPE_LABELS[item.Type] || item.Type;
  const ns = item.Namespace ? ` (${item.Namespace})` : '';
  const version =
    item.Version !== undefined && item.Version !== null
      ? ` v${item.Version}`
      : '';
  return `${type} ${item.ID}${version}${ns}`;
}
