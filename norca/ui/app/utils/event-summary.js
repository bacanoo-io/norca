/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Turns raw event stream events into display items (title, text, tone,
// icon, link). Used by the notifications menu and the dashboard timeline.

const TONE_COLORS = {
  success: 'green',
  info: 'blue',
  warning: 'orange',
  danger: 'red',
  neutral: 'secondary',
};

function shortId(id) {
  return id ? String(id).split('-')[0] : '';
}

function jobLink(namespace, jobId) {
  if (!jobId) return {};
  return {
    route: 'jobs.job.index',
    model: `${jobId}@${namespace || 'default'}`,
  };
}

function deploymentItem(event) {
  const d = event.Payload?.Deployment || {};
  const status = d.Status || event.Type;
  let tone = 'info';
  if (status === 'successful') tone = 'success';
  if (status === 'failed' || status === 'cancelled') tone = 'danger';
  if (status === 'paused' || status === 'blocked') tone = 'warning';
  return {
    icon: 'git-branch',
    tone,
    title: `Deployment of ${d.JobID || 'a job'} ${status}`,
    text: d.StatusDescription || `Version ${d.JobVersion ?? '?'}`,
    notable: tone !== 'info',
    ...jobLink(d.Namespace, d.JobID),
  };
}

function allocationItem(event) {
  const a = event.Payload?.Allocation || {};
  const status = a.ClientStatus || 'updated';
  let tone = 'neutral';
  if (status === 'running') tone = 'success';
  if (status === 'failed' || status === 'lost') tone = 'danger';
  if (status === 'pending') tone = 'info';
  return {
    icon: 'box',
    tone,
    title: `Allocation ${shortId(a.ID)} ${status}`,
    text: [a.JobID, a.TaskGroup].filter(Boolean).join(' / '),
    notable: tone === 'danger',
    ...(a.ID ? { route: 'allocations.allocation', model: a.ID } : {}),
  };
}

function evaluationItem(event) {
  const e = event.Payload?.Evaluation || {};
  const status = e.Status || 'updated';
  let tone = 'neutral';
  if (status === 'blocked') tone = 'warning';
  if (status === 'failed') tone = 'danger';
  if (status === 'complete') tone = 'success';
  return {
    icon: 'list-details',
    tone,
    title: `Evaluation ${shortId(e.ID)} ${status}`,
    text: [e.JobID, e.TriggeredBy].filter(Boolean).join(', '),
    notable: tone === 'warning' || tone === 'danger',
    ...jobLink(e.Namespace, e.JobID),
  };
}

function nodeItem(event) {
  const n = event.Payload?.Node || {};
  const status = n.Status || event.Type;
  let tone = 'neutral';
  if (status === 'ready') tone = 'success';
  if (status === 'down') tone = 'danger';
  if (status === 'disconnected' || n.Drain) tone = 'warning';
  return {
    icon: 'server-2',
    tone,
    title: `Client ${n.Name || shortId(n.ID)} ${n.Drain ? 'draining' : status}`,
    text: event.Type,
    notable: tone === 'danger' || tone === 'warning',
    ...(n.ID ? { route: 'clients.client', model: n.ID } : {}),
  };
}

function jobItem(event) {
  const j = event.Payload?.Job || {};
  const deregistered = event.Type === 'JobDeregistered';
  return {
    icon: 'briefcase',
    tone: deregistered ? 'neutral' : 'info',
    title: `Job ${j.ID || event.Key} ${
      deregistered ? 'stopped' : 'registered'
    }`,
    text: j.Version !== undefined ? `Version ${j.Version}` : event.Type,
    notable: false,
    ...jobLink(j.Namespace || event.Namespace, j.ID || event.Key),
  };
}

// A decision of the built-in autoscaler (ScalingDecision) or a scale up
// capped by the quota of the namespace (ScalingQuotaCapped).
function scalingItem(event) {
  const d = event.Payload || {};
  const capped = event.Type === 'ScalingQuotaCapped';
  return {
    icon: 'arrows-maximize',
    tone: capped ? 'warning' : 'info',
    title: capped
      ? `Scaling of ${d.JobID}/${d.Group} capped by quota ${
          d.Quota || ''
        }`.trim()
      : `${d.JobID}/${d.Group} scaled from ${d.PreviousCount} to ${d.Count}${
          d.DryRun ? ' (dry run)' : ''
        }`,
    text: d.Reason || (d.Check ? `check ${d.Check}` : ''),
    notable: capped,
    ...jobLink(d.Namespace || event.Namespace, d.JobID),
  };
}

const BUILDERS = {
  Scaling: scalingItem,
  Deployment: deploymentItem,
  Allocation: allocationItem,
  Evaluation: evaluationItem,
  Node: nodeItem,
  Job: jobItem,
};

export function summarizeEvent(event) {
  const build = BUILDERS[event.Topic];
  const item = build
    ? build(event)
    : {
        icon: 'activity',
        tone: 'neutral',
        title: `${event.Topic}: ${event.Type}`,
        text: event.Key || '',
        notable: false,
      };
  return {
    ...item,
    color: TONE_COLORS[item.tone] || 'secondary',
    index: event.Index,
    region: event.Region,
    key: `${event.Region || ''}:${event.Index}:${event.Topic}:${event.Key}:${
      event.Type
    }`,
  };
}
