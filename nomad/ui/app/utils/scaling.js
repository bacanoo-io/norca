/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';
import { apiRequest } from 'nomad-ui/utils/norca-api';

// Scaling (development plan, autoscaler): the policies with their engine
// (built-in on the leader, or an external autoscaler), the job scale status
// with the last 20 scaling events of each group, and the live decisions of
// the built-in engine on the Scaling topic of the event stream.

export function listPolicies(token, region, params = {}) {
  return apiRequest(token, '/v1/scaling/policies', {
    params: { region, ...params },
  });
}

export function getPolicy(token, region, id, namespace) {
  return apiRequest(token, `/v1/scaling/policy/${encodeURIComponent(id)}`, {
    params: { region, namespace },
  });
}

export function jobScale(token, region, job, namespace) {
  return apiRequest(token, `/v1/job/${encodeURIComponent(job)}/scale`, {
    params: { region, namespace },
  });
}

export function formatNs(ns) {
  if (!ns) return '–';
  const seconds = Math.round(ns / 1e9);
  if (seconds % 60 === 0 && seconds >= 60) return `${seconds / 60} min`;
  return `${seconds} s`;
}

// A persisted ScalingEvent (job scale status) as a timeline item.
export function eventItem(event, { job, group, namespace, region }) {
  const meta = event.Meta || {};
  const capped = !!meta['nomad_autoscaler.count.capped'];
  const dryRun = !!meta['nomad_autoscaler.dry_run'];
  const hasCount = event.Count !== null && event.Count !== undefined;
  let title = `${job} / ${group}`;
  if (hasCount) title += `: ${event.PreviousCount} → ${event.Count}`;
  else if (dryRun) title += ': dry run';
  else title += event.Error ? ': error' : ': recorded';
  let color = 'blue';
  if (event.Error) color = 'red';
  else if (capped) color = 'orange';
  else if (!hasCount) color = 'secondary';
  const time = event.Time ? new Date(event.Time / 1e6) : null;
  return {
    icon: 'arrows-maximize',
    color,
    title,
    text: event.Message || '',
    time: time ? moment(time).fromNow() : '',
    timestamp: time ? time.getTime() : 0,
    engine: meta['nomad_autoscaler.engine'] || '',
    capped,
    error: !!event.Error,
    count: hasCount ? event.Count : null,
    previous: event.PreviousCount,
    route: 'jobs.job.index',
    model: `${job}@${namespace || 'default'}`,
    region,
  };
}

// A live ScalingDecision / ScalingQuotaCapped event as a timeline item.
export function decisionItem(event) {
  const d = event.Payload || {};
  const capped = event.Type === 'ScalingQuotaCapped';
  const time = d.Time ? new Date(d.Time / 1e6) : new Date();
  let title = `${d.JobID} / ${d.Group}: ${d.PreviousCount} → ${d.Count}`;
  if (d.DryRun) title = `${d.JobID} / ${d.Group}: dry run (${d.DesiredCount})`;
  return {
    icon: 'arrows-maximize',
    color: capped ? 'orange' : 'blue',
    title,
    text: capped
      ? `Capped from ${d.DesiredCount} to ${d.Count} by quota ${d.Quota}`
      : [d.Check && `check ${d.Check}`, d.Reason].filter(Boolean).join(': '),
    time: moment(time).fromNow(),
    timestamp: time.getTime(),
    engine: 'builtin',
    live: true,
    route: 'jobs.job.index',
    model: `${d.JobID}@${d.Namespace || 'default'}`,
    region: event.Region,
  };
}
