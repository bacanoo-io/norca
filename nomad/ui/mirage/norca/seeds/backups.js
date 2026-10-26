/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection } from '../util';

const hour = 3600 * 1e9;

function iso(minutesAgo) {
  return new Date(Date.now() - minutesAgo * 60000).toISOString();
}

function id(minutesAgo, index) {
  const d = new Date(Date.now() - minutesAgo * 60000);
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getUTCFullYear()}${p(d.getUTCMonth() + 1)}${p(
    d.getUTCDate()
  )}T${p(d.getUTCHours())}${p(d.getUTCMinutes())}00Z-${index}`;
}

function destinations(status = 'verified') {
  return [
    {
      Name: 'primary',
      Status: status,
      Error: status === 'failed' ? 'upload: connection reset' : '',
      Location: 's3://norca-backup/eu/',
      EncryptionMode: 'age',
      RecipientsFingerprint: 'age1q8x...f3k',
      Objects: {
        state: { Key: 'state.bin.age', Size: 48 * 1024 * 1024, SHA256: 'ab12' },
        keyring: { Key: 'keyring.age', Size: 4096, SHA256: 'cd34' },
      },
      Duration: 12 * 1e9,
    },
    {
      Name: 'dr-copy',
      Status: 'verified',
      Error: '',
      Location: 's3://norca-dr/eu/',
      EncryptionMode: 'age',
      RecipientsFingerprint: 'age1z0p...9wq',
      Objects: {
        state: { Key: 'state.bin.age', Size: 48 * 1024 * 1024, SHA256: 'ab12' },
        keyring: { Key: 'keyring.age', Size: 4096, SHA256: 'cd34' },
      },
      Duration: 19 * 1e9,
    },
  ];
}

function job(idName, version, modifyIndex, extra = {}) {
  return {
    ID: idName,
    Namespace: 'default',
    Name: idName,
    Type: 'service',
    Status: 'running',
    Stop: false,
    Version: version,
    Versions: Array.from(
      { length: Math.min(version + 1, 4) },
      (_, i) => version - i
    ),
    SubmitTime: Date.now() * 1e6,
    JobModifyIndex: modifyIndex,
    ModifyIndex: modifyIndex,
    ...extra,
  };
}

function contents(versionOffset = 0) {
  return {
    Namespaces: [
      { Name: 'default', Description: 'Default shared namespace', Quota: '' },
      { Name: 'prod', Description: 'Production', Quota: 'team-a' },
    ],
    Jobs: [
      job('api', 40 + versionOffset, 900 + versionOffset),
      job('worker', 17, 820),
      job('report-export', 6, 700, { Type: 'batch', Status: 'dead' }),
    ],
    JobVersions: [
      {
        ID: 'api',
        Namespace: 'default',
        Version: 40 + versionOffset,
        Type: 'service',
        Status: 'running',
        SubmitTime: Date.now() * 1e6,
      },
      {
        ID: 'api',
        Namespace: 'default',
        Version: 39 + versionOffset,
        Type: 'service',
        Status: 'running',
        SubmitTime: (Date.now() - 86400000) * 1e6,
      },
    ],
    Variables: [
      {
        Namespace: 'default',
        Path: 'nomad/jobs/api',
        Keys: ['db_password', 'api_key'],
        CreateTime: 1,
        ModifyTime: 2,
        ModifyIndex: 310,
      },
      {
        Namespace: 'default',
        Path: 'nomad/jobs/worker',
        Keys: ['queue_url'],
        CreateTime: 1,
        ModifyTime: 2,
        ModifyIndex: 320,
      },
    ],
    ACLPolicies: [
      {
        Name: 'api-reader',
        Description: 'Read the api job',
        CreateIndex: 10,
        ModifyIndex: 12,
      },
    ],
    ACLRoles: [
      {
        ID: 'r-1',
        Name: 'operators',
        Description: 'On-call operators',
        Policies: [{ Name: 'api-reader' }],
        CreateIndex: 11,
        ModifyIndex: 11,
      },
    ],
    Deployments: [
      {
        ID: 'd1a2b3c4-0000-0000-0000-000000000001',
        Namespace: 'default',
        JobID: 'api',
        JobVersion: 40 + versionOffset,
        Status: 'successful',
        StatusDescription: 'Deployment completed successfully',
        ModifyIndex: 905,
        TaskGroups: {
          web: { DesiredTotal: 6, HealthyAllocs: 6, UnhealthyAllocs: 0 },
        },
      },
    ],
    Allocations: [
      {
        ID: 'a1b2c3d4-0000-0000-0000-000000000001',
        Namespace: 'default',
        JobID: 'api',
        JobVersion: 40 + versionOffset,
        TaskGroup: 'web',
        Name: 'api.web[0]',
        NodeName: 'client-1',
        ClientStatus: 'running',
        DesiredStatus: 'run',
        ModifyIndex: 910,
      },
      {
        ID: 'a1b2c3d4-0000-0000-0000-000000000002',
        Namespace: 'default',
        JobID: 'api',
        JobVersion: 39 + versionOffset,
        TaskGroup: 'web',
        Name: 'api.web[1]',
        NodeName: 'client-2',
        ClientStatus: 'complete',
        DesiredStatus: 'stop',
        ModifyIndex: 880,
      },
    ],
  };
}

/**
 * Seeds the backup API with a schedule, four backups (one pinned, one
 * failed) and the current state for comparisons.
 *   seedBackups(server, { overdue: true })
 */
export default function seedBackups(
  server,
  { region = 'global', overdue = false, empty = false, enabled = true } = {}
) {
  const db = server.db;
  collection(db, 'norcaBackupSchedules').insert({
    Region: region,
    Enabled: enabled,
    Interval: hour,
    Schedule: '',
    Destinations: ['primary', 'dr-copy'],
    LastSuccess: empty ? '0001-01-01T00:00:00Z' : iso(overdue ? 200 : 12),
    LastSuccessID: empty ? '' : id(overdue ? 200 : 12, 1834567),
    LastAttempt: empty ? '0001-01-01T00:00:00Z' : iso(12),
    LastError: overdue ? 'primary: upload: connection reset' : '',
    NextDue: enabled ? iso(-48) : '0001-01-01T00:00:00Z',
    Overdue: overdue,
  });
  collection(db, 'norcaBackupCurrent').insert({
    RaftIndex: 2000,
    Contents: {
      ...contents(2),
      Jobs: [
        job('api', 42, 990),
        job('worker', 17, 820),
        job('new-service', 1, 980),
      ],
    },
  });
  if (empty) return;
  const backups = collection(db, 'norcaBackups');
  const base = {
    Region: region,
    AuthoritativeRegion: region,
    NorcaVersion: '0.1.0',
    NomadVersion: '1.6.5',
    RaftTerm: 3,
    LeaderID: 'server-1',
    KeyIDs: ['key-1'],
    Summary: {
      namespaces: 2,
      jobs: 3,
      job_versions: 8,
      variables: 2,
      acl_policies: 1,
      acl_roles: 1,
      deployments: 1,
      allocations: 2,
    },
  };
  [
    [overdue ? 200 : 12, 1834567, 'schedule', 'complete', false, {}, 0],
    [72, 1834400, 'schedule', 'complete', false, {}, 0],
    [
      1200,
      1820000,
      'manual',
      'complete',
      true,
      { note: 'before the upgrade' },
      -1,
    ],
    [1500, 1810000, 'schedule', 'failed', false, {}, -2],
  ].forEach(([ago, index, trigger, status, pinned, labels, offset]) => {
    backups.insert({
      ...base,
      ID: id(ago, index),
      Trigger: trigger,
      Status: status,
      StatusDescription: status === 'failed' ? 'all destinations failed' : '',
      StartedAt: iso(ago + 1),
      CompletedAt: iso(ago),
      RaftIndex: index,
      Path: `2026/10/24/${id(ago, index)}`,
      Destinations: destinations(status === 'failed' ? 'failed' : 'verified'),
      Labels: labels,
      Pinned: pinned,
      CreateIndex: index,
      ModifyIndex: index,
      Contents: contents(offset),
    });
  });
}
