/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';

// Quotas (kubernetes-features-spec.md, item 1): a QuotaSpec has one limit
// per region; the usage of a region is reported by that region's servers
// (QuotaUsage.Used, keyed by the hash of the limit). In a limit 0 (or null)
// means unlimited and a negative value means disallowed.

export const DIMENSIONS = [
  {
    id: 'cpu',
    label: 'CPU',
    unit: 'MHz',
    get: (l) => l?.RegionLimit?.CPU,
    set: (l, v) => (l.RegionLimit = { ...(l.RegionLimit || {}), CPU: v }),
  },
  {
    id: 'memory',
    label: 'Memory',
    unit: 'MB',
    get: (l) => l?.RegionLimit?.MemoryMB,
    set: (l, v) => (l.RegionLimit = { ...(l.RegionLimit || {}), MemoryMB: v }),
  },
  {
    id: 'memory_max',
    label: 'Memory max',
    unit: 'MB',
    get: (l) => l?.RegionLimit?.MemoryMaxMB,
    set: (l, v) =>
      (l.RegionLimit = { ...(l.RegionLimit || {}), MemoryMaxMB: v }),
  },
  {
    id: 'allocations',
    label: 'Allocations',
    unit: '',
    get: (l) => l?.Allocations,
    set: (l, v) => (l.Allocations = v),
  },
  {
    id: 'variables',
    label: 'Variables',
    unit: 'MiB',
    get: (l) => l?.VariablesLimit,
    set: (l, v) => (l.VariablesLimit = v),
  },
];

export const NEARLY_FULL = 0.9;

function format(value, unit) {
  const n = Number(value || 0).toLocaleString('en-US');
  return unit ? `${n} ${unit}` : n;
}

// The rows of a limit: { label, used, limit, percent, state, text, segments }.
// Dimensions that are unlimited and unused are left out.
export function usageRows(limit, used) {
  return DIMENSIONS.map((d) => {
    const max = d.get(limit);
    const value = d.get(used) || 0;
    if (!max && !value) return null;
    if (max < 0) {
      return {
        id: d.id,
        label: d.label,
        state: 'disallowed',
        text: 'not allowed',
        percent: 0,
        segments: [],
      };
    }
    if (!max) {
      return {
        id: d.id,
        label: d.label,
        state: 'unlimited',
        text: `${format(value, d.unit)} used, no limit`,
        percent: 0,
        segments: [],
      };
    }
    const ratio = value / max;
    const percent = Math.round(ratio * 100);
    let color = 'green';
    if (ratio >= NEARLY_FULL) color = 'red';
    else if (ratio >= 0.75) color = 'orange';
    return {
      id: d.id,
      label: d.label,
      used: value,
      limit: max,
      percent,
      ratio,
      state: ratio >= 1 ? 'full' : ratio >= NEARLY_FULL ? 'nearly' : 'ok',
      text: `${format(value, d.unit)} of ${format(max, d.unit)} (${percent}%)`,
      segments: [
        { label: 'used', value: Math.min(value, max), color },
        { label: 'free', value: Math.max(0, max - value), color: 'secondary' },
      ],
    };
  }).filter(Boolean);
}

// The limit of a spec for a region, and its usage.
export function limitFor(spec, region) {
  const limits = spec?.Limits || [];
  return (
    limits.find((l) => l.Region === region) ||
    (region ? null : limits[0]) ||
    null
  );
}

export function usedFor(usage, limit) {
  if (!usage || !limit) return null;
  const used = usage.Used || {};
  return (
    used[limit.Hash] ||
    Object.values(used).find((u) => u.Region === limit.Region) ||
    null
  );
}

export function listQuotas(token, region) {
  return apiRequest(token, '/v1/quotas', { params: { region } });
}

export function listUsages(token, region) {
  return apiRequest(token, '/v1/quota-usages', { params: { region } });
}

export function getQuota(token, region, name) {
  return apiRequest(token, `/v1/quota/${encodeURIComponent(name)}`, {
    params: { region },
  });
}

export function saveQuota(token, region, spec) {
  return apiRequest(token, `/v1/quota/${encodeURIComponent(spec.Name)}`, {
    method: 'PUT',
    params: { region },
    data: spec,
  });
}

export function deleteQuota(token, region, name) {
  return apiRequest(token, `/v1/quota/${encodeURIComponent(name)}`, {
    method: 'DELETE',
    params: { region },
  });
}

// Evaluations blocked by a quota, with the exhausted dimensions of their
// failed task groups.
export function quotaBlocked(evaluations = []) {
  return evaluations
    .filter((e) => e.Status === 'blocked' && e.QuotaLimitReached)
    .map((e) => ({
      ...e,
      dimensions: [
        ...new Set(
          Object.values(e.FailedTGAllocs || {}).flatMap(
            (m) => m.QuotaExhausted || []
          )
        ),
      ],
    }));
}
