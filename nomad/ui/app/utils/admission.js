/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';

// Admission policies (kubernetes-features-spec.md, item 2).

export function matchText(match) {
  const parts = [];
  const ns = match?.Namespaces || [];
  parts.push(ns.length ? `namespaces ${ns.join(', ')}` : 'every namespace');
  const res = match?.Resources || [];
  parts.push(res.length ? res.join(', ') : 'jobs');
  if (match?.VariablePaths?.length)
    parts.push(`paths ${match.VariablePaths.join(', ')}`);
  return parts.join('; ');
}

export function denialItem(event) {
  const d = event.Payload || {};
  const time = moment();
  return {
    icon: 'shield-check',
    color: 'red',
    title: `${d.Operation === 'plan' ? 'Plan' : 'Submission'} of ${
      d.JobID || d.Path
    } denied`,
    text: [
      `${d.Controller === 'webhook' ? 'webhook' : 'policy'} ${d.Name}`,
      d.Message,
      d.User?.Name && `by ${d.User.Name}`,
    ]
      .filter(Boolean)
      .join(': '),
    time: time.format('HH:mm:ss'),
    name: d.Name,
  };
}
