/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Response } from 'miragejs';
import { collection, body, plain, error, json, inNamespace } from './util';

// Mirage routes of deployments per region. While norcaDeployments has
// records (Deployment JSON plus Region and Allocations), the deployment
// routes answer from it, per ?region=; otherwise they keep the behaviour of
// the inherited routes (Ember Data shaped deployment models).

function own(db) {
  const c = collection(db, 'norcaDeployments');
  return c.length ? c : null;
}

function inRegion(record, request) {
  const region = request.queryParams.region;
  return !region || (record.Region || 'global') === region;
}

function strip(record) {
  // eslint-disable-next-line no-unused-vars
  const { Region, Allocations, ...rest } = plain(record);
  return rest;
}

export default function registerDeploymentRoutes(server) {
  server.get('/deployments', function (schema, request) {
    const namespace = request.queryParams.namespace || 'default';
    const mine = own(schema.db);
    if (mine)
      return json(
        mine
          .where((d) => inRegion(d, request) && inNamespace(d, namespace))
          .map(strip)
      );
    const { deployments, jobs } = schema;
    return this.serialize(deployments.all())
      .map((deployment) => ({
        ...deployment,
        Namespace:
          deployment.Namespace ||
          jobs.find(deployment.JobID)?.namespaceId ||
          'default',
      }))
      .filter((d) => namespace === '*' || d.Namespace === namespace);
  });

  server.get('/deployment/allocations/:id', function (schema, request) {
    const mine = own(schema.db);
    if (mine) {
      const d = mine.findBy({ ID: request.params.id });
      return json(d?.Allocations || []);
    }
    const job = schema.jobs.find(
      schema.deployments.find(request.params.id).jobId
    );
    const allocations = schema.allocations.where({ jobId: job.id });
    return this.serialize(allocations.slice(0, 3));
  });

  server.get('/deployment/:id', function (schema, request) {
    const mine = own(schema.db);
    if (mine) {
      const d = mine.where(
        (x) => x.ID === request.params.id && inRegion(x, request)
      )[0];
      return d ? json(strip(d)) : error(404, 'deployment not found');
    }
    return this.serialize(schema.deployments.find(request.params.id));
  });

  server.get('/job/:id/deployment', function (schema, request) {
    const mine = own(schema.db);
    if (mine) {
      const namespace = request.queryParams.namespace || 'default';
      const d = mine
        .where(
          (x) =>
            x.JobID === request.params.id &&
            inRegion(x, request) &&
            inNamespace(x, namespace)
        )
        .sort((a, b) => b.CreateIndex - a.CreateIndex)[0];
      return d ? json(strip(d)) : new Response(200, {}, 'null');
    }
    const deployment = schema.deployments.where({ jobId: request.params.id })
      .models[0];
    return deployment
      ? this.serialize(deployment)
      : new Response(200, {}, 'null');
  });

  const act = (name, change) =>
    function (schema, request) {
      const mine = own(schema.db);
      if (!mine) return new Response(204, {}, '');
      const d = mine.where((x) => x.ID === request.params.id)[0];
      if (!d) return error(404, 'deployment not found');
      const payload = body(request);
      if (payload.DeploymentID && payload.DeploymentID !== d.ID)
        return error(400, 'Deployment ID does not match');
      const result = change(d, payload);
      if (typeof result === 'string') return error(500, result);
      mine.update(d.id, { ...result, ModifyIndex: (d.ModifyIndex || 0) + 1 });
      return json({
        EvalID: `eval-${name}`,
        EvalCreateIndex: 1,
        DeploymentModifyIndex: (d.ModifyIndex || 0) + 1,
        RevertedJobVersion: null,
        Index: 1,
      });
    };
  const register = (name, change) => {
    server.post(`/deployment/${name}/:id`, act(name, change));
    server.put(`/deployment/${name}/:id`, act(name, change));
  };
  register('run', (d) =>
    d.Status === 'pending'
      ? { Status: 'running', StatusDescription: 'Deployment is running' }
      : `deployment ${d.ID} is ${d.Status}, not pending`
  );
  register('unblock', (d) =>
    d.Status === 'blocked' || d.Status === 'unblocking'
      ? {
          Status: 'successful',
          StatusDescription: 'Deployment completed successfully',
        }
      : `deployment ${d.ID} is not blocked`
  );
  register('fail', () => ({
    Status: 'failed',
    StatusDescription: 'Deployment marked as failed',
  }));
  register('pause', (d, p) => ({
    Status: p.Pause ? 'paused' : 'running',
    StatusDescription: p.Pause
      ? 'Deployment is paused'
      : 'Deployment is running',
  }));
  register('promote', (d) => {
    const groups = {};
    Object.entries(d.TaskGroups || {}).forEach(([k, g]) => {
      groups[k] = { ...g, Promoted: true };
    });
    return { TaskGroups: groups };
  });
}

function deployment(region, id, status, extra = {}) {
  return {
    Region: region,
    ID: id,
    Namespace: 'default',
    JobID: 'api',
    JobVersion: 42,
    JobModifyIndex: 100,
    JobSpecModifyIndex: 100,
    JobCreateIndex: 10,
    IsMultiregion: true,
    TaskGroups: {
      web: {
        AutoRevert: true,
        AutoPromote: false,
        ProgressDeadline: 600e9,
        RequireProgressBy: '0001-01-01T00:00:00Z',
        Promoted: false,
        PlacedCanaries: null,
        DesiredCanaries: 0,
        DesiredTotal: 6,
        PlacedAllocs: 6,
        HealthyAllocs: 6,
        UnhealthyAllocs: 0,
      },
    },
    Status: status,
    StatusDescription: '',
    EvalPriority: 50,
    CreateIndex: 200,
    ModifyIndex: 210,
    Allocations: [],
    ...extra,
  };
}

/**
 * Seeds a multiregion deployment of job api v42 over eu-west (done,
 * blocked), us-east (running with canaries) and ap-south (pending), and a
 * finished single-region deployment of worker. The job itself needs
 * Multiregion: see the acceptance test.
 */
export function seedDeployments(server) {
  const c = collection(server.db, 'norcaDeployments');
  c.insert(
    deployment('eu-west', 'd-eu-0000-0000-0000-000000000001', 'blocked', {
      StatusDescription: 'Deployment is complete but waiting for peer region',
    })
  );
  c.insert(
    deployment('us-east', 'd-us-0000-0000-0000-000000000002', 'running', {
      StatusDescription: 'Deployment is running but requires manual promotion',
      TaskGroups: {
        web: {
          AutoRevert: true,
          AutoPromote: false,
          ProgressDeadline: 600e9,
          Promoted: false,
          PlacedCanaries: ['a1', 'a2'],
          DesiredCanaries: 2,
          DesiredTotal: 6,
          PlacedAllocs: 4,
          HealthyAllocs: 3,
          UnhealthyAllocs: 0,
        },
      },
      Allocations: [
        {
          ID: 'a1000000-0000-0000-0000-000000000001',
          Name: 'api.web[0]',
          TaskGroup: 'web',
          NodeName: 'us-client-1',
          ClientStatus: 'running',
          DeploymentStatus: { Healthy: true, Canary: true },
          JobVersion: 42,
        },
      ],
    })
  );
  c.insert(
    deployment('ap-south', 'd-ap-0000-0000-0000-000000000003', 'pending', {
      StatusDescription: 'Deployment is pending, waiting for peer region',
      TaskGroups: {
        web: {
          DesiredTotal: 4,
          PlacedAllocs: 0,
          HealthyAllocs: 0,
          UnhealthyAllocs: 0,
          DesiredCanaries: 0,
        },
      },
    })
  );
  c.insert(
    deployment('eu-west', 'd-wk-0000-0000-0000-000000000004', 'successful', {
      JobID: 'worker',
      JobVersion: 17,
      IsMultiregion: false,
      StatusDescription: 'Deployment completed successfully',
      CreateIndex: 150,
    })
  );
}

export const MULTIREGION = {
  Strategy: { MaxParallel: 1, OnFailure: 'fail_all' },
  Regions: [
    {
      Name: 'eu-west',
      Count: 6,
      Datacenters: ['eu-1'],
      NodePool: 'default',
      Meta: null,
    },
    {
      Name: 'us-east',
      Count: 6,
      Datacenters: ['us-1'],
      NodePool: 'default',
      Meta: null,
    },
    {
      Name: 'ap-south',
      Count: 4,
      Datacenters: ['ap-1'],
      NodePool: 'default',
      Meta: null,
    },
  ],
};
