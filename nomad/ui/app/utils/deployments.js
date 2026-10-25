/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { apiRequest } from 'nomad-ui/utils/norca-api';
import { deploymentProgress } from 'nomad-ui/utils/dashboard-data';

// Deployments across jobs and regions, and the per-region rollout of a
// multiregion deployment. Every region keeps its own deployment (with its
// own ID); they belong together by namespace, job and job version. The
// rollout order and strategy are on the job (Multiregion).

export const ACTIVE = [
  'running',
  'paused',
  'pending',
  'initializing',
  'blocked',
  'unblocking',
];

export function listDeployments(token, region, namespace) {
  return apiRequest(token, '/v1/deployments', {
    params: { region, namespace },
  });
}

export function getDeployment(token, region, id) {
  return apiRequest(token, `/v1/deployment/${encodeURIComponent(id)}`, {
    params: { region },
  });
}

export function deploymentAllocations(token, region, id) {
  return apiRequest(
    token,
    `/v1/deployment/allocations/${encodeURIComponent(id)}`,
    { params: { region } }
  );
}

export function getJob(token, region, job, namespace) {
  return apiRequest(token, `/v1/job/${encodeURIComponent(job)}`, {
    params: { region, namespace },
  });
}

export function latestDeployment(token, region, job, namespace) {
  return apiRequest(token, `/v1/job/${encodeURIComponent(job)}/deployment`, {
    params: { region, namespace },
  });
}

// run, unblock, fail, pause (with { Pause }), promote (with { All: true })
export function deploymentAction(token, region, action, id, extra = {}) {
  return apiRequest(
    token,
    `/v1/deployment/${action}/${encodeURIComponent(id)}`,
    {
      method: 'POST',
      params: { region },
      data: { DeploymentID: id, ...extra },
    }
  );
}

export function progressSegments(deployment) {
  const p = deploymentProgress(deployment);
  return {
    ...p,
    segments: [
      { label: 'healthy', value: p.healthy, color: 'green' },
      { label: 'unhealthy', value: p.unhealthy, color: 'red' },
      {
        label: 'remaining',
        value: Math.max(0, p.desired - p.healthy - p.unhealthy),
        color: 'secondary',
      },
    ],
  };
}

export function needsPromotion(deployment) {
  return Object.values(deployment?.TaskGroups || {}).some(
    (g) => g.DesiredCanaries > 0 && !g.Promoted
  );
}

export const ON_FAILURE = {
  '': 'the regions after the failed one fail',
  fail_all: 'every region fails',
  fail_local: 'only the failed region fails',
};

/**
 * The per-region state of a multiregion deployment: one step per region of
 * the job's Multiregion, in rollout order, with that region's deployment of
 * the same version (or none yet, or a newer one that supersedes it).
 */
export async function multiregionRollout(
  token,
  { job, namespace, version, homeRegion }
) {
  const { body: spec } = await getJob(token, homeRegion, job, namespace);
  const multiregion = spec?.Multiregion;
  if (!multiregion) return null;
  const steps = await Promise.all(
    (multiregion.Regions || []).map(async (r, index) => {
      let deployment = null;
      let error = null;
      try {
        deployment = (await latestDeployment(token, r.Name, job, namespace))
          .body;
      } catch (e) {
        error = e.status === 403 ? 'not permitted' : 'unreachable';
      }
      let state = 'none';
      if (deployment && deployment.IsMultiregion) {
        if (deployment.JobVersion === version) state = 'current';
        else if (deployment.JobVersion > version) state = 'superseded';
      }
      return {
        index,
        region: r.Name,
        count: r.Count,
        datacenters: (r.Datacenters || []).join(', '),
        deployment: state === 'current' ? deployment : null,
        state,
        error,
        progress: state === 'current' ? progressSegments(deployment) : null,
      };
    })
  );
  return {
    strategy: multiregion.Strategy || {},
    steps,
  };
}
