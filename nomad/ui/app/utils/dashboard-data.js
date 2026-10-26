/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { usageRows, limitFor, usedFor } from 'nomad-ui/utils/quotas';
import { normalize as normalizeRelease } from 'nomad-ui/utils/releases';

// Data of the dashboard: one summary per region, built from the list APIs
// (status/leader, agent/members, nodes, jobs with summaries, deployments,
// evaluations). The fetches go through the token service's raw request so
// every region is queried explicitly with ?region=.

const EMPTY_ALLOCS = {
  running: 0,
  pending: 0,
  failed: 0,
  lost: 0,
  complete: 0,
};

export function withParams(path, params) {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '')
      search.set(key, value);
  }
  const qs = search.toString();
  return qs ? `${path}?${qs}` : path;
}

async function getJSON(request, path, params) {
  const response = await request(withParams(path, params));
  if (!response.ok) {
    const error = new Error(
      `${path}: ${response.status} ${response.statusText || ''}`.trim()
    );
    error.status = response.status;
    throw error;
  }
  const index = Number(response.headers?.get?.('X-Nomad-Index')) || 0;
  return { body: await response.json(), index };
}

// Optional parts of a summary: a 403 (the token cannot list clients, for
// example) leaves that part empty instead of failing the region.
async function optional(promise, fallback) {
  try {
    return await promise;
  } catch (e) {
    if (e.status === 403) return { body: fallback, index: 0, forbidden: true };
    throw e;
  }
}

export function summarizeClients(nodes = []) {
  const clients = {
    total: nodes.length,
    ready: 0,
    down: 0,
    disconnected: 0,
    draining: 0,
    ineligible: 0,
  };
  for (const node of nodes) {
    if (node.Status === 'ready') clients.ready++;
    if (node.Status === 'down') clients.down++;
    if (node.Status === 'disconnected') clients.disconnected++;
    if (node.Drain) clients.draining++;
    if (node.SchedulingEligibility === 'ineligible') clients.ineligible++;
  }
  return clients;
}

export function summarizeAllocations(jobs = []) {
  const allocs = { ...EMPTY_ALLOCS };
  for (const job of jobs) {
    const groups = job.JobSummary?.Summary || {};
    for (const group of Object.values(groups)) {
      allocs.running += group.Running || 0;
      allocs.pending += (group.Starting || 0) + (group.Queued || 0);
      allocs.failed += group.Failed || 0;
      allocs.lost += group.Lost || 0;
      allocs.complete += group.Complete || 0;
    }
  }
  return allocs;
}

export function summarizeServers(members = [], region) {
  // Members carry their region in the tags; without tags (or without a known
  // region) all members are counted.
  const tagged = members.some((m) => m.Tags?.region);
  const own =
    tagged && region
      ? members.filter((m) => m.Tags?.region === region)
      : members;
  return {
    total: own.length,
    alive: own.filter((m) => m.Status === 'alive').length,
    members: own,
  };
}

export function leaderName(leader, servers) {
  if (!leader) return null;
  const [address, port] = String(leader).split(/:(?=\d+$)/);
  const member = servers.members.find(
    (m) =>
      (m.Addr || m.Address) === address &&
      (!port || String(m.Tags?.port) === port)
  );
  return member
    ? member.Name.replace(new RegExp(`\\.${member.Tags?.region}$`), '')
    : leader;
}

// The latest deployment of every job, keyed by namespace and job.
function latestDeployments(deployments = []) {
  const latest = new Map();
  for (const d of deployments) {
    const key = `${d.Namespace}/${d.JobID}`;
    const current = latest.get(key);
    if (!current || (d.CreateIndex || 0) > (current.CreateIndex || 0))
      latest.set(key, d);
  }
  return [...latest.values()];
}

export function deploymentProgress(deployment) {
  let desired = 0;
  let healthy = 0;
  let unhealthy = 0;
  for (const group of Object.values(deployment.TaskGroups || {})) {
    desired += group.DesiredTotal || 0;
    healthy += group.HealthyAllocs || 0;
    unhealthy += group.UnhealthyAllocs || 0;
  }
  return { desired, healthy, unhealthy };
}

export async function fetchRegionSummary(
  request,
  { region, namespace, members }
) {
  const params = { region, namespace: namespace || '*' };
  const [
    leader,
    nodes,
    jobs,
    deployments,
    evaluations,
    backups,
    quotas,
    usages,
    releases,
  ] = await Promise.all([
    getJSON(request, '/v1/status/leader', { region }).catch(() => ({
      body: null,
      index: 0,
    })),
    optional(getJSON(request, '/v1/nodes', { region }), []),
    optional(getJSON(request, '/v1/jobs', { ...params, meta: 'true' }), []),
    optional(getJSON(request, '/v1/deployments', params), []),
    optional(
      getJSON(request, '/v1/evaluations', {
        ...params,
        filter: 'Status == "blocked"',
      }),
      []
    ),
    // the backup schedule: any failure (no permission) leaves it out
    getJSON(request, '/v1/operator/backups', { region }).catch(() => ({
      body: null,
      index: 0,
    })),
    // quotas and their usage in the region: left out without permission
    getJSON(request, '/v1/quotas', { region }).catch(() => ({
      body: null,
      index: 0,
    })),
    getJSON(request, '/v1/quota-usages', { region }).catch(() => ({
      body: null,
      index: 0,
    })),
    // releases with drift or failed hooks
    getJSON(request, '/v1/releases', params).catch(() => ({
      body: null,
      index: 0,
    })),
  ]);

  const servers = summarizeServers(members, region);
  const latest = latestDeployments(deployments.body || []);
  return {
    region,
    leader: leader.body,
    leaderName: leaderName(leader.body, servers),
    servers,
    clients: summarizeClients(nodes.body || []),
    clientsForbidden: !!nodes.forbidden,
    allocations: summarizeAllocations(jobs.body || []),
    jobs: (jobs.body || []).length,
    runningDeployments: latest
      .filter(
        (d) =>
          d.Status === 'running' ||
          d.Status === 'pending' ||
          d.Status === 'paused'
      )
      .map((d) => ({ ...d, Region: region, progress: deploymentProgress(d) })),
    failedDeployments: latest
      .filter((d) => d.Status === 'failed')
      .map((d) => ({ ...d, Region: region })),
    blockedEvaluations: (evaluations.body || []).filter(
      (e) => e.Status === 'blocked'
    ),
    backup: summarizeBackups(backups.body),
    quotas: nearlyFullQuotas(quotas.body, usages.body, region),
    releases: releaseProblems(releases.body),
    // the newest index of the fetched state, to resume the event stream after
    index: Math.max(
      nodes.index,
      jobs.index,
      deployments.index,
      evaluations.index
    ),
    error: null,
  };
}

// The backup schedule of a region: last success, overdue (older than two
// intervals), the last error. Null when the token cannot read backups.
export function summarizeBackups(body) {
  const schedule = body?.Schedule;
  if (!schedule) return null;
  const set = (t) => t && !String(t).startsWith('0001-');
  return {
    enabled: !!schedule.Enabled,
    lastSuccess: set(schedule.LastSuccess) ? schedule.LastSuccess : null,
    overdue: !!schedule.Overdue,
    lastError: schedule.LastError || '',
  };
}

// The quota dimensions of the region at 90 % or more of their limit.
export function nearlyFullQuotas(specs, usages, region) {
  const result = [];
  for (const spec of specs || []) {
    const limit = limitFor(spec, region);
    if (!limit) continue;
    const usage = (usages || []).find((u) => u.Name === spec.Name);
    for (const row of usageRows(limit, usedFor(usage, limit))) {
      if (row.state === 'nearly' || row.state === 'full')
        result.push({
          name: spec.Name,
          dimension: row.label,
          percent: row.percent,
        });
    }
  }
  return result;
}

// Releases that drifted from their revision or whose hooks failed.
export function releaseProblems(body) {
  const problems = [];
  for (const raw of body || []) {
    const r = normalizeRelease(raw);
    const conditions = r.Status?.Conditions || [];
    const has = (type, status) =>
      conditions.some((c) => c.Type === type && c.Status === status);
    if (has('Drifted', 'True'))
      problems.push({ kind: 'drift', name: r.Name, namespace: r.Namespace });
    if (has('HooksSucceeded', 'False'))
      problems.push({ kind: 'hook', name: r.Name, namespace: r.Namespace });
  }
  return problems;
}

export function unreachableRegion(region, error) {
  return {
    region,
    leader: null,
    leaderName: null,
    servers: { total: 0, alive: 0, members: [] },
    clients: summarizeClients([]),
    allocations: { ...EMPTY_ALLOCS },
    jobs: 0,
    runningDeployments: [],
    failedDeployments: [],
    blockedEvaluations: [],
    backup: null,
    quotas: [],
    releases: [],
    index: 0,
    error: error?.message || String(error),
  };
}

// Warnings across the regions, most severe first; each says what to do.
export function buildWarnings(summaries) {
  const warnings = [];
  for (const s of summaries) {
    const where = s.region || 'the local region';
    const Region = s.region ? `Region ${s.region}` : 'The local region';
    if (s.error) {
      warnings.push({
        tone: 'danger',
        title: `${Region} cannot be reached.`,
        text: `${s.error}. Check the federation connection and the servers of the region.`,
        route: 'servers',
      });
      continue;
    }
    if (!s.leader) {
      warnings.push({
        tone: 'danger',
        title: `${Region} has no leader.`,
        text: 'Check that a quorum of servers is running and can reach each other.',
        route: 'servers',
      });
    }
    if (s.servers.total && s.servers.alive < s.servers.total) {
      const n = s.servers.total - s.servers.alive;
      warnings.push({
        tone: 'danger',
        title: `${n} ${
          n === 1 ? 'server is' : 'servers are'
        } not alive in ${where}.`,
        text: 'Restart the server agents or remove the failed members to restore fault tolerance.',
        route: 'servers',
      });
    }
    if (s.clients.down) {
      warnings.push({
        tone: 'warning',
        title: `${s.clients.down} ${
          s.clients.down === 1 ? 'client is' : 'clients are'
        } down in ${where}.`,
        text: 'Their allocations are rescheduled to other clients. Bring the clients back or remove them.',
        route: 'clients',
      });
    }
    if (s.clients.disconnected) {
      warnings.push({
        tone: 'warning',
        title: `${s.clients.disconnected} ${
          s.clients.disconnected === 1 ? 'client is' : 'clients are'
        } disconnected in ${where}.`,
        text: 'Allocations of disconnected clients are kept until max_client_disconnect expires.',
        route: 'clients',
      });
    }
    const quotaBlocked = s.blockedEvaluations.filter(
      (e) => e.QuotaLimitReached
    );
    for (const name of [
      ...new Set(quotaBlocked.map((e) => e.QuotaLimitReached)),
    ]) {
      const n = quotaBlocked.filter((e) => e.QuotaLimitReached === name).length;
      warnings.push({
        tone: 'warning',
        title: `${n} ${
          n === 1 ? 'evaluation is' : 'evaluations are'
        } blocked by quota ${name} in ${where}.`,
        text: 'They run when the quota frees up: stop other work of the namespaces or raise the limit.',
        route: 'quotas.quota',
        model: name,
      });
    }
    for (const q of s.quotas || []) {
      warnings.push({
        tone: q.percent >= 100 ? 'danger' : 'warning',
        title: `Quota ${q.name} is ${
          q.percent >= 100 ? 'full' : 'nearly full'
        } in ${where}: ${q.dimension} at ${q.percent}%.`,
        text: 'New work of its namespaces will wait or be rejected. Raise the limit or free resources.',
        route: 'quotas.quota',
        model: q.name,
      });
    }
    for (const r of s.releases || []) {
      warnings.push({
        tone: 'warning',
        title:
          r.kind === 'drift'
            ? `Release ${r.name} differs from its revision in ${where}.`
            : `A hook of release ${r.name} failed in ${where}.`,
        text:
          r.kind === 'drift'
            ? 'Its resources were changed outside the release. Roll back, or take the change into the pack variables and upgrade.'
            : 'Look at the hook job and its allocations, fix the cause and upgrade or roll back the release.',
        route: 'packages.release',
        models: [r.namespace || 'default', r.name],
      });
    }
    const otherBlocked = s.blockedEvaluations.length - quotaBlocked.length;
    if (otherBlocked) {
      const n = otherBlocked;
      warnings.push({
        tone: 'warning',
        title: `${n} ${
          n === 1 ? 'evaluation is' : 'evaluations are'
        } blocked in ${where}.`,
        text: 'They wait for capacity: add clients, free resources or relax the placement constraints.',
        route: 'evaluations',
      });
    }
    if (s.backup?.overdue) {
      warnings.push({
        tone: 'warning',
        title: `The last successful backup of ${where} is overdue.`,
        text: `${
          s.backup.lastError ? `${s.backup.lastError}. ` : ''
        }Check the backup destinations and the server logs, then start a backup.`,
        route: 'backups',
      });
    }
    for (const d of s.failedDeployments) {
      warnings.push({
        tone: 'warning',
        title: `The latest deployment of ${d.JobID} failed in ${where}.`,
        text: `${
          d.StatusDescription || 'Deployment failed'
        }. Fix the job and run it again, or revert to a working version.`,
        route: 'jobs.job.deployments',
        model: `${d.JobID}@${d.Namespace || 'default'}`,
      });
    }
  }
  const order = { danger: 0, warning: 1, info: 2 };
  return warnings.sort((a, b) => order[a.tone] - order[b.tone]);
}

export function totals(summaries) {
  const sum = (fn) => summaries.reduce((acc, s) => acc + fn(s), 0);
  return {
    runningDeployments: sum((s) => s.runningDeployments.length),
    blockedEvaluations: sum((s) => s.blockedEvaluations.length),
    failedAllocations: sum((s) => s.allocations.failed + s.allocations.lost),
    clientsReady: sum((s) => s.clients.ready),
    clientsTotal: sum((s) => s.clients.total),
    allocations: {
      running: sum((s) => s.allocations.running),
      pending: sum((s) => s.allocations.pending),
      failed: sum((s) => s.allocations.failed),
      lost: sum((s) => s.allocations.lost),
      complete: sum((s) => s.allocations.complete),
    },
  };
}
