/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { get } from '@ember/object';
import {
  currentURL,
  fillIn,
  settled,
  triggerKeyEvent,
  waitUntil,
} from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import Topology from 'nomad-ui/tests/pages/topology';
import {
  formatBytes,
  formatScheduledBytes,
  formatHertz,
  formatScheduledHertz,
} from 'nomad-ui/utils/units';
import queryString from 'query-string';
import percySnapshot from '@percy/ember';
import faker from 'nomad-ui/mirage/faker';

function streamResponse(lines) {
  return Promise.resolve({
    status: 200,
    ok: true,
    body: null,
    text: () =>
      Promise.resolve(lines.map((l) => JSON.stringify(l)).join('\n') + '\n'),
  });
}

const sumResources = (list, dimension) =>
  list.reduce((agg, val) => agg + (get(val, dimension) || 0), 0);

module('Acceptance | topology', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    server.createList('node-pool', 5);
    server.create('job', { createAllocations: false });
  });

  test('it passes an accessibility audit', async function (assert) {
    assert.expect(1);

    server.createList('node', 3);
    server.createList('allocation', 5);

    await Topology.visit();
    await a11yAudit(assert);
  });

  test('by default the info panel shows cluster aggregate stats', async function (assert) {
    faker.seed(1);
    server.create('node-pool', { name: 'all' });
    server.createList('node', 3);
    server.createList('allocation', 5);

    await Topology.visit();

    await percySnapshot(assert);

    assert.equal(Topology.infoPanelTitle, 'Cluster Details');
    assert.notOk(Topology.filteredNodesWarning.isPresent);

    assert.equal(
      Topology.clusterInfoPanel.nodeCount,
      `${server.schema.nodes.all().length}`
    );

    const allocs = server.schema.allocations.all().models;
    const scheduledAllocs = allocs.filter((alloc) =>
      ['pending', 'running'].includes(alloc.clientStatus)
    );
    assert.equal(
      Topology.clusterInfoPanel.allocCount,
      `${scheduledAllocs.length}`
    );

    // Node pool count ignores 'all'.
    const nodePools = server.schema.nodePools
      .all()
      .models.filter((p) => p.name !== 'all');
    assert.equal(
      Topology.clusterInfoPanel.nodePoolCount,
      `${nodePools.length}`
    );

    const nodeResources = server.schema.nodes
      .all()
      .models.mapBy('nodeResources');
    const taskResources = scheduledAllocs
      .mapBy('taskResources.models')
      .flat()
      .mapBy('resources');

    const totalMem = sumResources(nodeResources, 'Memory.MemoryMB');
    const totalCPU = sumResources(nodeResources, 'Cpu.CpuShares');
    const reservedMem = sumResources(taskResources, 'Memory.MemoryMB');
    const reservedCPU = sumResources(taskResources, 'Cpu.CpuShares');

    assert.equal(
      Topology.clusterInfoPanel.memoryProgressValue,
      // the meter stops at full (overcommitted test data)
      Math.min(reservedMem / totalMem, 1)
    );
    assert.equal(
      Topology.clusterInfoPanel.cpuProgressValue,
      Math.min(reservedCPU / totalCPU, 1)
    );

    assert.equal(
      Topology.clusterInfoPanel.memoryAbsoluteValue,
      `${formatBytes(reservedMem * 1024 * 1024)} / ${formatBytes(
        totalMem * 1024 * 1024
      )} reserved`
    );

    assert.equal(
      Topology.clusterInfoPanel.cpuAbsoluteValue,
      `${formatHertz(reservedCPU, 'MHz')} / ${formatHertz(
        totalCPU,
        'MHz'
      )} reserved`
    );
  });

  test('all allocations for all namespaces and all clients are queried on load', async function (assert) {
    server.createList('node', 3);
    server.createList('allocation', 5);

    await Topology.visit();
    const requests = this.server.pretender.handledRequests;
    assert.ok(requests.findBy('url', '/v1/nodes?resources=true'));

    const allocationsRequest = requests.find((req) =>
      req.url.startsWith('/v1/allocations')
    );
    assert.ok(allocationsRequest);

    const allocationRequestParams = queryString.parse(
      allocationsRequest.url.split('?')[1]
    );
    assert.deepEqual(allocationRequestParams, {
      namespace: '*',
      task_states: 'false',
      resources: 'true',
    });
  });

  test('when an allocation is selected, the info panel shows information on the allocation', async function (assert) {
    const nodes = server.createList('node', 5);
    const job = server.create('job', { createAllocations: false });
    const taskGroup = server.schema.find('taskGroup', job.taskGroupIds[0]).name;
    const allocs = server.createList('allocation', 5, {
      forceRunningClientStatus: true,
      jobId: job.id,
      taskGroup,
    });

    // Get the first alloc of the first node that has an alloc
    const sortedNodes = nodes.sortBy('datacenter');
    let node, alloc;
    for (let n of sortedNodes) {
      alloc = allocs.find((a) => a.nodeId === n.id);
      if (alloc) {
        node = n;
        break;
      }
    }

    const dcIndex = nodes
      .mapBy('datacenter')
      .uniq()
      .sort()
      .indexOf(node.datacenter);
    const nodeIndex = nodes
      .filterBy('datacenter', node.datacenter)
      .indexOf(node);

    const reset = async () => {
      await Topology.visit();
      await Topology.viz.datacenters[dcIndex].nodes[
        nodeIndex
      ].memoryRects[0].select();
    };

    await reset();
    assert.equal(Topology.infoPanelTitle, 'Allocation Details');

    assert.equal(Topology.allocInfoPanel.id, alloc.id.split('-')[0]);

    const uniqueClients = allocs.mapBy('nodeId').uniq();
    assert.equal(Topology.allocInfoPanel.siblingAllocs, `${allocs.length}`);
    assert.equal(
      Topology.allocInfoPanel.uniquePlacements,
      `${uniqueClients.length}`
    );

    assert.equal(Topology.allocInfoPanel.job, job.name);
    assert.ok(Topology.allocInfoPanel.taskGroup.endsWith(alloc.taskGroup));
    assert.equal(Topology.allocInfoPanel.client, node.id.split('-')[0]);

    await Topology.allocInfoPanel.visitAlloc();
    assert.equal(currentURL(), `/allocations/${alloc.id}`);

    await reset();

    await Topology.allocInfoPanel.visitJob();
    assert.equal(currentURL(), `/jobs/${job.id}@default`);

    await reset();

    await Topology.allocInfoPanel.visitClient();
    assert.equal(currentURL(), `/clients/${node.id}`);
  });

  test('changing which allocation is selected changes the metric charts', async function (assert) {
    server.create('node');
    const job1 = server.create('job', { createAllocations: false });
    const taskGroup1 = server.schema.find(
      'taskGroup',
      job1.taskGroupIds[0]
    ).name;
    server.create('allocation', {
      forceRunningClientStatus: true,
      jobId: job1.id,
      taskGroup1,
    });

    const job2 = server.create('job', { createAllocations: false });
    const taskGroup2 = server.schema.find(
      'taskGroup',
      job2.taskGroupIds[0]
    ).name;
    server.create('allocation', {
      forceRunningClientStatus: true,
      jobId: job2.id,
      taskGroup2,
    });

    await Topology.visit();
    await Topology.viz.datacenters[0].nodes[0].memoryRects[0].select();
    const firstAllocationTaskNames =
      Topology.allocInfoPanel.charts[0].areas.mapBy('taskName');

    await Topology.viz.datacenters[0].nodes[0].memoryRects[1].select();
    const secondAllocationTaskNames =
      Topology.allocInfoPanel.charts[0].areas.mapBy('taskName');

    assert.notDeepEqual(firstAllocationTaskNames, secondAllocationTaskNames);
  });

  test('when a node is selected, the info panel shows information on the node', async function (assert) {
    // A high node count is required for node selection
    const nodes = server.createList('node', 51);
    const node = nodes.sortBy('datacenter')[0];
    server.createList('allocation', 5, { forceRunningClientStatus: true });

    const allocs = server.schema.allocations.where({ nodeId: node.id }).models;

    await Topology.visit();

    await Topology.viz.datacenters[0].nodes[0].selectNode();
    assert.equal(Topology.infoPanelTitle, 'Client Details');

    assert.equal(Topology.nodeInfoPanel.id, node.id.split('-')[0]);
    assert.equal(Topology.nodeInfoPanel.name, node.name);
    assert.equal(Topology.nodeInfoPanel.address, node.httpAddr);
    assert.equal(Topology.nodeInfoPanel.status, node.status);

    assert.equal(
      Topology.nodeInfoPanel.drainingLabel,
      node.drain ? 'Yes' : 'No'
    );
    assert.equal(
      Topology.nodeInfoPanel.eligibleLabel,
      node.schedulingEligibility === 'eligible' ? 'Yes' : 'No'
    );

    assert.equal(Topology.nodeInfoPanel.drainingIsAccented, node.drain);
    assert.equal(
      Topology.nodeInfoPanel.eligibleIsAccented,
      node.schedulingEligibility !== 'eligible'
    );

    const taskResources = allocs
      .mapBy('taskResources.models')
      .flat()
      .mapBy('resources');
    const reservedMem = sumResources(taskResources, 'Memory.MemoryMB');
    const reservedCPU = sumResources(taskResources, 'Cpu.CpuShares');

    const totalMem = node.nodeResources.Memory.MemoryMB;
    const totalCPU = node.nodeResources.Cpu.CpuShares;

    assert.equal(
      Topology.nodeInfoPanel.memoryProgressValue,
      Math.min(reservedMem / totalMem, 1)
    );
    assert.equal(
      Topology.nodeInfoPanel.cpuProgressValue,
      Math.min(reservedCPU / totalCPU, 1)
    );

    assert.equal(
      Topology.nodeInfoPanel.memoryAbsoluteValue,
      `${formatScheduledBytes(
        reservedMem * 1024 * 1024
      )} / ${formatScheduledBytes(totalMem, 'MiB')} reserved`
    );

    assert.equal(
      Topology.nodeInfoPanel.cpuAbsoluteValue,
      `${formatScheduledHertz(reservedCPU, 'MHz')} / ${formatScheduledHertz(
        totalCPU,
        'MHz'
      )} reserved`
    );

    await Topology.nodeInfoPanel.visitNode();
    assert.equal(currentURL(), `/clients/${node.id}`);
  });

  test('when one or more nodes lack the NodeResources property, a warning message is shown', async function (assert) {
    server.createList('node', 3);
    server.createList('allocation', 5);

    server.schema.nodes.all().models[0].update({ nodeResources: null });

    await Topology.visit();
    assert.ok(Topology.filteredNodesWarning.isPresent);
    assert.ok(Topology.filteredNodesWarning.message.startsWith('1'));
  });

  test('Filtering and Querying reduces the number of nodes shown', async function (assert) {
    server.createList('node', 10);
    server.createList('node', 2, {
      nodeClass: 'foo-bar-baz',
    });

    // Make sure we have at least one node draining and one ineligible.
    server.create('node', {
      schedulingEligibility: 'ineligible',
    });
    server.create('node', 'draining');

    // Create node pool exclusive for these nodes.
    server.create('node-pool', { name: 'test-node-pool' });
    server.createList('node', 3, {
      nodePool: 'test-node-pool',
    });

    server.createList('allocation', 5);

    // Count draining and ineligible nodes.
    const counts = {
      ineligible: 0,
      draining: 0,
    };
    server.db.nodes.forEach((n) => {
      if (n.schedulingEligibility === 'ineligible') {
        counts['ineligible'] += 1;
      }
      if (n.drain) {
        counts['draining'] += 1;
      }
    });

    await Topology.visit();
    assert.dom('[data-test-topo-viz-node]').exists({ count: 17 });

    // Test search.
    const name = server.schema.nodes.first().name;
    await fillIn('[data-test-topology-search]', name);
    assert.dom('[data-test-topo-viz-node]').exists({ count: 1 });
    await fillIn('[data-test-topology-search]', name + name);
    assert.dom('[data-test-topo-viz-node]').doesNotExist();
    await fillIn('[data-test-topology-search]', '');
    assert.dom('[data-test-topo-viz-node]').exists({ count: 17 });

    // Test node class filter.
    await Topology.facets.class.toggle();
    await Topology.facets.class.options
      .findOneBy('label', 'foo-bar-baz')
      .toggle();
    assert.dom('[data-test-topo-viz-node]').exists({ count: 2 });
    await Topology.facets.class.options
      .findOneBy('label', 'foo-bar-baz')
      .toggle();

    // Test ineligible state filter.
    await Topology.facets.state.toggle();
    await Topology.facets.state.options
      .findOneBy('label', 'Ineligible')
      .toggle();
    assert
      .dom('[data-test-topo-viz-node]')
      .exists({ count: counts['ineligible'] });
    await Topology.facets.state.options
      .findOneBy('label', 'Ineligible')
      .toggle();
    await Topology.facets.state.toggle();

    // Test draining state filter.
    await Topology.facets.state.toggle();
    await Topology.facets.state.options.findOneBy('label', 'Draining').toggle();
    assert
      .dom('[data-test-topo-viz-node]')
      .exists({ count: counts['draining'] });
    await Topology.facets.state.options.findOneBy('label', 'Draining').toggle();
    await Topology.facets.state.toggle();

    // Test node pool filter.
    await Topology.facets.nodePool.toggle();
    await Topology.facets.nodePool.options
      .findOneBy('label', 'test-node-pool')
      .toggle();
    assert.dom('[data-test-topo-viz-node]').exists({ count: 3 });
    await Topology.facets.nodePool.options
      .findOneBy('label', 'test-node-pool')
      .toggle();
  });

  test('a change of a client or an allocation in the event stream reloads the topology', async function (assert) {
    server.createList('node', 2);
    server.createList('allocation', 3);

    const eventStream = this.owner.lookup('service:event-stream');
    const urls = [];
    eventStream.enabled = true;
    // The first connection that asks for the clients gets one event; every
    // other request stays open.
    let served = false;
    eventStream.request = (url) => {
      urls.push(url);
      if (served || !url.includes('topic=Node')) return new Promise(() => {});
      served = true;
      return streamResponse([
        {
          Index: 100,
          Events: [
            {
              Topic: 'Node',
              Type: 'NodeRegistration',
              Key: server.db.nodes[0].id,
              Index: 100,
              Payload: { Node: { ID: server.db.nodes[0].id } },
            },
          ],
        },
      ]);
    };

    await Topology.visit();
    const nodeRequests = () =>
      server.pretender.handledRequests.filter((r) =>
        r.url.startsWith('/v1/nodes')
      ).length;
    await waitUntil(() => urls.some((url) => url.includes('topic=Node')), {
      timeout: 3000,
    });
    // The first load, then the reload after the event.
    await waitUntil(() => nodeRequests() >= 2, { timeout: 3000 });
    const streamURL = urls.find((url) => url.includes('topic=Node'));
    assert.ok(streamURL, 'it subscribes to the clients');
    assert.ok(streamURL.includes('topic=Allocation'), 'and to the allocations');
    assert.ok(nodeRequests() >= 2, 'the topology is reloaded');

    for (const { connection } of eventStream.connections.values())
      connection.stop();
    await settled();
  });

  test('allocations are keyboard operable', async function (assert) {
    server.createList('node', 1);
    server.createList('allocation', 2, { clientStatus: 'running' });

    await Topology.visit();
    const bar = document.querySelector('[data-test-memory-rect]');
    assert.dom(bar).hasAttribute('tabindex', '0');
    assert.dom(bar).hasAttribute('role', 'button');

    await triggerKeyEvent(bar, 'keydown', 'Enter');
    assert.equal(Topology.infoPanelTitle, 'Allocation Details');
  });
});
