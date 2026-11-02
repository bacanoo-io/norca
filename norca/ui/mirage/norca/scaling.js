/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Response } from 'miragejs';
import { collection, plain, error, json } from './util';

// Mirage routes of the scaling policy API. Policies live in
// norcaScalingPolicies (the full ScalingPolicy JSON); a job scale status
// in norcaJobScales ({ JobID, Namespace, TaskGroups }) takes precedence over
// the job-scale models of the inherited Mirage setup.

const DEFAULT_ENGINE = 'external';

function stub(p) {
  return {
    ID: p.ID,
    Enabled: p.Enabled,
    Type: p.Type,
    Engine: p.Engine,
    Target: p.Target,
    CreateIndex: p.CreateIndex,
    ModifyIndex: p.ModifyIndex,
  };
}

export default function registerScalingRoutes(server) {
  let scaleIndex = 1;
  server.get('/scaling/policies', function ({ db }, { queryParams }) {
    const { namespace = 'default', job, engine, type } = queryParams;
    if (engine && engine !== 'builtin' && engine !== 'external')
      return error(400, `invalid engine "${engine}"`);
    return json(
      collection(db, 'norcaScalingPolicies')
        .where((p) => {
          const ns = p.Target?.Namespace || 'default';
          if (namespace !== '*' && ns !== namespace) return false;
          if (job && p.Target?.Job !== job) return false;
          if (type && p.Type !== type) return false;
          if (engine && (p.Engine || DEFAULT_ENGINE) !== engine) return false;
          return true;
        })
        .map(stub)
    );
  });

  server.get('/scaling/policy/:id', function ({ db }, { params }) {
    const p = collection(db, 'norcaScalingPolicies').findBy({ ID: params.id });
    if (!p) return error(404, 'policy not found');
    return json(plain(p));
  });

  server.get('/job/:id/scale', function (schema, { params }) {
    const own = collection(schema.db, 'norcaJobScales').findBy({
      JobID: params.id,
    });
    // the inherited handler answered with an index that grows per request
    scaleIndex += 1;
    if (own) return json(plain(own), scaleIndex);
    return new Response(
      200,
      { 'x-nomad-index': String(scaleIndex) },
      this.serialize(schema.jobScales.findBy({ jobId: params.id }))
    );
  });
}

const ns = (ms) => ms * 1e6;

/**
 * Seeds two policies (built-in engine on api/web, external on
 * worker/queue) with their job scale status and scaling events.
 */
export function seedScaling(server) {
  const now = Date.now();
  const policies = collection(server.db, 'norcaScalingPolicies');
  const scales = collection(server.db, 'norcaJobScales');
  policies.insert({
    ID: 'policy-api-web',
    Type: 'horizontal',
    Target: { Namespace: 'default', Job: 'api', Group: 'web' },
    Policy: {
      evaluation_interval: '30s',
      cooldown: '2m',
      check: {
        cpu: {
          source: 'nomad-apm',
          query: 'avg_cpu',
          strategy: { 'target-value': { target: 70 } },
        },
      },
    },
    Min: 3,
    Max: 20,
    Enabled: true,
    Engine: 'builtin',
    EvaluationInterval: 30e9,
    Cooldown: 120e9,
    CooldownOnScaleUp: 0,
    OnCheckError: 'fail',
    ScaleDuringDeployment: 'never',
    Checks: [
      {
        Name: 'cpu',
        Source: 'nomad-apm',
        Metric: 'nomad.cpu',
        Query: 'avg_cpu',
        QueryWindow: 60e9,
        Strategy: 'target-value',
        StrategyConfig: { target: '70' },
        OnError: '',
      },
    ],
    CreateIndex: 10,
    ModifyIndex: 12,
  });
  policies.insert({
    ID: 'policy-worker-queue',
    Type: 'horizontal',
    Target: { Namespace: 'default', Job: 'worker', Group: 'queue' },
    Policy: { evaluation_interval: '1m' },
    Min: 2,
    Max: 12,
    Enabled: false,
    Engine: '',
    EvaluationInterval: 60e9,
    Cooldown: 0,
    Checks: null,
    CreateIndex: 11,
    ModifyIndex: 11,
  });
  scales.insert({
    JobID: 'api',
    Namespace: 'default',
    JobStopped: false,
    TaskGroups: {
      web: {
        Desired: 6,
        Placed: 6,
        Running: 6,
        Healthy: 6,
        Unhealthy: 0,
        Events: [
          {
            Time: ns(now - 5 * 60000),
            Count: 6,
            PreviousCount: 4,
            Message: 'scaling up because the average cpu is 82%, target 70%',
            Error: false,
            Meta: {
              'nomad_autoscaler.engine': 'builtin',
              nomad_policy_id: 'policy-api-web',
            },
            EvalID: 'eval-1',
          },
          {
            Time: ns(now - 45 * 60000),
            Count: 4,
            PreviousCount: 5,
            Message:
              'scaling down because the average cpu is 41%, target 70% (capped from 3 to 4 by quota "team-a")',
            Error: false,
            Meta: {
              'nomad_autoscaler.engine': 'builtin',
              'nomad_autoscaler.count.capped': true,
              'nomad_autoscaler.count.original': 3,
            },
            EvalID: 'eval-2',
          },
          {
            Time: ns(now - 90 * 60000),
            Count: null,
            PreviousCount: 5,
            Message:
              'autoscaler: scaling direction changed 4 times within 10m; policy suspended for 15m',
            Error: true,
            Meta: { nomad_policy_id: 'policy-api-web' },
            EvalID: null,
          },
        ],
      },
    },
  });
  scales.insert({
    JobID: 'worker',
    Namespace: 'default',
    JobStopped: false,
    TaskGroups: {
      queue: {
        Desired: 8,
        Placed: 8,
        Running: 8,
        Healthy: 8,
        Unhealthy: 0,
        Events: null,
      },
    },
  });
}
