/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Reading the metrics store of the servers (/v1/metrics/*): the history of
// the task group usage, of the cluster and of the quotas.

// The ranges of the history charts. `from` is a duration before now, in the
// form the API takes.
export const RANGES = [
  { id: '1h', label: '1 hour', from: '1h', seconds: 3600 },
  { id: '24h', label: '24 hours', from: '24h', seconds: 86400 },
  { id: '7d', label: '7 days', from: '168h', seconds: 7 * 86400 },
  { id: '30d', label: '30 days', from: '720h', seconds: 30 * 86400 },
];

export function rangeById(id) {
  return RANGES.find((r) => r.id === id) || RANGES[0];
}

/**
 * rangesFor returns the ranges a store with the retention (nanoseconds, as
 * the API returns a duration) holds: none longer than the retention. Without
 * a retention (not known yet, or an older server) all ranges.
 */
export function rangesFor(retention) {
  if (!(retention > 0)) return RANGES;
  const seconds = retention / 1e9;
  const ranges = RANGES.filter((r) => r.seconds <= seconds);
  return ranges.length ? ranges : [RANGES[0]];
}

/**
 * clampRange returns the range of the id among the ranges, or the longest of
 * them when the id is not one (a range longer than the retention).
 */
export function clampRange(id, ranges) {
  return ranges.find((r) => r.id === id) || ranges[ranges.length - 1];
}

/**
 * isHostMetric tells whether a metric is kept in the hosts store of the
 * servers, with its own retention (host_retention): the host statistics of
 * the clients and the internals of the servers and their system bus.
 */
export function isHostMetric(metric) {
  return /^norca\.(node|server|bus)\./.test(metric || '');
}

// MetricsForbidden is thrown when the token may not read the metrics.
export class MetricsForbidden extends Error {}

async function errorText(response) {
  const text = await response.text().catch(() => '');
  return text.trim() || `${response.status} ${response.statusText}`;
}

/**
 * fetchMetrics GETs /v1/metrics/<path> with the given query parameters
 * (empty values are left out) and returns the decoded body.
 */
export function fetchMetrics(token, path, params = {}) {
  return fetchJSON(token, `/v1/metrics/${path}`, params);
}

/**
 * fetchJSON GETs the URL with the given query parameters (empty values are
 * left out) and returns the decoded body; MetricsForbidden on a 403.
 */
export async function fetchJSON(token, url, params = {}) {
  const query = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== null && value !== '') {
      query.set(key, `${value}`);
    }
  });
  const qs = query.toString();
  const response = await token.authorizedRequest(`${url}${qs ? `?${qs}` : ''}`);
  if (response.status === 403) {
    throw new MetricsForbidden('Permission denied');
  }
  if (!response.ok) {
    throw new Error(await errorText(response));
  }
  return response.json();
}

/**
 * points returns the [time, value] pairs of a history (prop Avg or Max),
 * rounded to two decimals.
 */
export function points(history, prop = 'Avg') {
  return ((history && history.Points) || []).map((p) => [
    Date.parse(p.Time),
    Math.round(p[prop] * 100) / 100,
  ]);
}

/**
 * recommend rounds a measured peak up with 20% headroom to a step, the
 * suggestion of the right-sizing view.
 */
export function recommend(peak, step) {
  if (!(peak > 0)) return 0;
  return Math.ceil((peak * 1.2) / step) * step;
}
