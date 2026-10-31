/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';

// Services and networking (consul-independence-plan.md; K8s item 7): the
// native service catalog with health, service intentions (allow or deny by
// mTLS identity), network policies of a namespace and admin network
// policies of the cluster.

// An instance takes part in discovery when it is ready and not critical
// (or when its service includes instances that are not ready).
export function inDiscovery(instance) {
  if (instance.Discovery?.IncludeNotReady) return true;
  return instance.Ready !== false && instance.Health !== 'critical';
}

export function healthSummary(instances = []) {
  const s = {
    total: instances.length,
    passing: 0,
    warning: 0,
    critical: 0,
    unknown: 0,
    discoverable: 0,
  };
  for (const i of instances) {
    const h = i.Health || 'unknown';
    s[h] = (s[h] || 0) + 1;
    if (inDiscovery(i)) s.discoverable++;
  }
  s.excluded = s.total - s.discoverable;
  s.tone = s.critical
    ? 'danger'
    : s.warning || s.discoverable < s.total
    ? 'warning'
    : s.total
    ? 'success'
    : 'neutral';
  return s;
}

export function mtlsMode(instances = []) {
  const modes = new Set(
    instances.map((i) => i.Meta?.['norca-mtls']).filter(Boolean)
  );
  return [...modes].join(', ');
}

export function listServices(token, region, namespace) {
  return apiRequest(token, '/v1/services', { params: { region, namespace } });
}

export function serviceInstances(token, region, name, namespace) {
  return apiRequest(token, `/v1/service/${encodeURIComponent(name)}`, {
    params: { region, namespace },
  });
}

// External services: endpoints (an address) and domains (a DNS alias)
// registered without a job.
export function listExternalServices(token, region, namespace) {
  return apiRequest(token, '/v1/services/external', {
    params: { region, namespace },
  });
}

export function saveExternalService(token, region, namespace, svc) {
  return apiRequest(
    token,
    `/v1/service/external/${encodeURIComponent(svc.Name)}`,
    { method: 'PUT', params: { region, namespace }, data: svc }
  );
}

export function deleteExternalService(token, region, namespace, id) {
  return apiRequest(token, `/v1/service/external/${encodeURIComponent(id)}`, {
    method: 'DELETE',
    params: { region, namespace },
  });
}

export function listNodes(token, region) {
  return apiRequest(token, '/v1/nodes', { params: { region } });
}

export function listIntentions(token, region, namespace) {
  return apiRequest(token, '/v1/service-intentions', {
    params: { region, namespace },
  });
}

export function saveIntention(token, region, intention) {
  return apiRequest(token, '/v1/service-intention', {
    method: 'PUT',
    params: { region, namespace: intention.Namespace },
    data: intention,
  });
}

export function deleteIntention(token, region, i) {
  return apiRequest(token, '/v1/service-intention', {
    method: 'DELETE',
    params: {
      region,
      namespace: i.Namespace,
      source: i.Source,
      destination: i.Destination,
      source_namespace: i.SourceNamespace,
    },
  });
}

export function listNetworkPolicies(token, region, namespace) {
  return apiRequest(token, '/v1/network-policies', {
    params: { region, namespace },
  });
}

export function listAdminPolicies(token, region) {
  return apiRequest(token, '/v1/admin-network-policies', {
    params: { region },
  });
}

export function policyPath(admin, name) {
  return `/v1/${
    admin ? 'admin-network-policy' : 'network-policy'
  }/${encodeURIComponent(name)}`;
}

export function getPolicy(token, region, { admin, name, namespace }) {
  return apiRequest(token, policyPath(admin, name), {
    params: { region, namespace: admin ? undefined : namespace },
  });
}

export function policyStatus(token, region, { admin, name, namespace }) {
  return apiRequest(token, `${policyPath(admin, name)}/status`, {
    params: { region, namespace: admin ? undefined : namespace },
  });
}

export function deletePolicy(token, region, { admin, name, namespace }) {
  return apiRequest(token, policyPath(admin, name), {
    method: 'DELETE',
    params: { region, namespace: admin ? undefined : namespace },
  });
}

export function selectorText(selector) {
  const entries = Object.entries(selector || {});
  return entries.length
    ? entries.map(([k, v]) => `${k}=${v}`).join(', ')
    : 'all workloads';
}

// The rules of a policy as rows: direction, name, action, peers, ports.
export function ruleRows(policy) {
  const rows = [];
  for (const [direction, rules] of [
    ['ingress', policy?.Ingress],
    ['egress', policy?.Egress],
  ]) {
    for (const r of rules || []) {
      rows.push({
        direction,
        name: r.Name || '–',
        action: r.Action || 'allow',
        peers:
          (r.Peers || [])
            .map((p) => {
              const parts = [];
              if (p.CIDR?.length) parts.push(p.CIDR.join(', '));
              if (p.Namespace) parts.push(`namespace ${p.Namespace}`);
              if (p.Selector && Object.keys(p.Selector).length)
                parts.push(selectorText(p.Selector));
              return parts.join(' ');
            })
            .join('; ') || 'any',
        ports: (r.Ports || []).join(', ') || 'all ports',
      });
    }
  }
  return rows;
}
