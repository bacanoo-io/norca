/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';

import {
  click,
  visit,
  find,
  findAll,
  fillIn,
  settled,
  triggerEvent,
} from '@ember/test-helpers';

import { setupMirage } from 'ember-cli-mirage/test-support';
import faker from 'nomad-ui/mirage/faker';
import percySnapshot from '@percy/ember';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

// The allocation row groups allocations by the width it has. The tests set
// that width instead of depending on the size of the page frame: the row
// gets `width` pixels, the panel keeps its own padding around it.
async function setAllocationRowWidth(width) {
  const panel = find('.norca-job-status');
  const row = find('.norca-alloc-status-row > div');
  panel.style.width = `${panel.clientWidth - row.clientWidth + width}px`;
  await triggerEvent(window, 'resize');
}
// TODO: Mirage is not type-friendly / assigns "server" as a global. Try to work around this shortcoming.

module('Acceptance | job status panel', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(async function () {
    server.create('node-pool');
    server.create('node');
  });

  test('Status panel lets you switch between Current and Historical', async function (assert) {
    assert.expect(5);
    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      createAllocations: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    await a11yAudit(assert, STRICT);
    await percySnapshot(assert, {
      percyCSS: `
        .allocation-row td { display: none; }
      `,
    });

    assert
      .dom('[data-test-status-mode="current"]')
      .exists('Current mode by default');

    await click('[data-test-status-mode-current]');

    assert
      .dom('[data-test-status-mode="current"]')
      .exists('Clicking active mode makes no change');

    await click('[data-test-status-mode-historical]');

    assert
      .dom('[data-test-status-mode="historical"]')
      .exists('Lets you switch to historical mode');
  });

  test('Status panel observes query parameters for current/historical', async function (assert) {
    assert.expect(2);
    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      createAllocations: true,
      noActiveDeployment: true,
    });

    await visit(`/jobs/${job.id}?statusMode=historical`);
    assert.dom('.norca-job-status').exists();

    assert
      .dom('[data-test-status-mode="historical"]')
      .exists('Historical mode when rendered with queryParams');
  });

  test('Status Panel shows accurate number and types of ungrouped allocation blocks', async function (assert) {
    assert.expect(7);

    faker.seed(1);

    let groupTaskCount = 10;

    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 1,
        failed: 0,
        unknown: 0,
        lost: 0,
      },
      groupTaskCount,
      shallow: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();

    let jobAllocCount = server.db.allocations.where({
      jobId: job.id,
    }).length;

    assert.equal(
      jobAllocCount,
      groupTaskCount * job.taskGroups.length,
      'Correect number of allocs generated (metatest)'
    );
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists(
        { count: jobAllocCount },
        `All ${jobAllocCount} allocations are represented in the status panel`
      );

    groupTaskCount = 20;

    job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 0.5,
        failed: 0.5,
        unknown: 0,
        lost: 0,
      },
      groupTaskCount,
      noActiveDeployment: true,
      shallow: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();

    let runningAllocCount = server.db.allocations.where({
      jobId: job.id,
      clientStatus: 'running',
    }).length;

    let failedAllocCount = server.db.allocations.where({
      jobId: job.id,
      clientStatus: 'failed',
    }).length;

    assert.equal(
      runningAllocCount + failedAllocCount,
      groupTaskCount * job.taskGroups.length,
      'Correect number of allocs generated (metatest)'
    );
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists(
        { count: runningAllocCount },
        `All ${runningAllocCount} running allocations are represented in the status panel`
      );
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-failed')
      .exists(
        { count: failedAllocCount },
        `All ${failedAllocCount} failed allocations are represented in the status panel`
      );
    await percySnapshot(assert, {
      percyCSS: `
          .allocation-row td { display: none; }
        `,
    });
  });

  test('After running/pending allocations are covered, fill in allocs by jobVersion, descending', async function (assert) {
    assert.expect(9);
    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: false,
      groupTaskCount: 4,
      shallow: true,
      version: 5,
    });

    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'running',
      jobVersion: 5,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'pending',
      jobVersion: 5,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'running',
      jobVersion: 3,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'failed',
      jobVersion: 4,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'lost',
      jobVersion: 5,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();

    // We expect to see 4 represented-allocations, since that's the number in our groupTaskCount
    assert.dom('.norca-alloc-blocks .norca-alloc').exists({ count: 4 });

    // We expect 2 of them to be running, and one to be pending, since running/pending allocations superecede other clientStatuses
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists({ count: 2 });
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-pending')
      .exists({ count: 1 });

    // We expect the lone other allocation to be lost, since it has the highest jobVersion
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-lost')
      .exists({ count: 1 });

    // We expect the job versions legend to show 3 at v5 (running, pending, and lost), and 1 at v3 (old running), and none at v4 (failed is not represented)
    assert
      .dom('.norca-job-status .norca-versions > ul > li')
      .exists({ count: 2 });
    assert
      .dom('.norca-job-status .norca-versions > ul > li > a[data-version="5"]')
      .exists({ count: 1 });
    assert
      .dom('.norca-job-status .norca-versions > ul > li > a[data-version="3"]')
      .exists({ count: 1 });
    assert
      .dom('.norca-job-status .norca-versions > ul > li > a[data-version="4"]')
      .doesNotExist();
    await percySnapshot(assert, {
      percyCSS: `
        .allocation-row td { display: none; }
      `,
    });
  });

  test('After running/pending allocations are covered, fill in allocs by jobVersion, descending (batch)', async function (assert) {
    assert.expect(7);
    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'batch',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: false,
      allocStatusDistribution: {
        running: 0.5,
        failed: 0.3,
        unknown: 0,
        lost: 0,
        complete: 0.2,
      },
      groupTaskCount: 5,
      shallow: true,
      version: 5,
      noActiveDeployment: true,
    });

    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'running',
      jobVersion: 5,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'pending',
      jobVersion: 5,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'running',
      jobVersion: 3,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'failed',
      jobVersion: 4,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'complete',
      jobVersion: 4,
    });
    server.create('allocation', {
      jobId: job.id,
      clientStatus: 'lost',
      jobVersion: 5,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    // We expect to see 5 represented-allocations, since that's the number in our groupTaskCount
    assert.dom('.norca-alloc-blocks .norca-alloc').exists({ count: 5 });

    // We expect 2 of them to be running, and one to be pending, since running/pending allocations superecede other clientStatuses
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists({ count: 2 });
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-pending')
      .exists({ count: 1 });

    // We expect 1 to be lost, since it has the highest jobVersion
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-lost')
      .exists({ count: 1 });

    // We expect the remaining one to be complete, rather than failed, since it comes earlier in the jobAllocStatuses.batch constant
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-complete')
      .exists({ count: 1 });
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-failed')
      .doesNotExist();

    await percySnapshot(assert, {
      percyCSS: `
        .allocation-row td { display: none; }
      `,
    });
  });

  test('Status Panel groups allocations when they get past a threshold', async function (assert) {
    assert.expect(6);

    faker.seed(1);

    let groupTaskCount = 20;

    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 1,
        failed: 0,
        unknown: 0,
        lost: 0,
      },
      groupTaskCount,
      shallow: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    await setAllocationRowWidth(920);

    let jobAllocCount = server.db.allocations.where({
      jobId: job.id,
    }).length;

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists(
        { count: jobAllocCount },
        `All ${jobAllocCount} allocations are represented in the status panel, ungrouped`
      );

    groupTaskCount = 40;

    job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 1,
        failed: 0,
        unknown: 0,
        lost: 0,
      },
      groupTaskCount,
      shallow: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    await setAllocationRowWidth(920);

    jobAllocCount = server.db.allocations.where({
      jobId: job.id,
    }).length;

    // At the standard row width, 40 allocations will attempt to display 20 ungrouped, and 20 grouped.
    let desiredUngroupedAllocCount = 20;
    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists(
        { count: desiredUngroupedAllocCount },
        `${desiredUngroupedAllocCount} allocations are represented ungrouped`
      );

    assert
      .dom('.norca-alloc.norca-alloc-rest')
      .exists('Allocations are numerous enough that a summary block exists');
    assert
      .dom('.norca-alloc.norca-alloc-rest')
      .hasText(
        `+${groupTaskCount - desiredUngroupedAllocCount}`,
        'Summary block has the correct number of grouped allocs'
      );

    await percySnapshot(assert, {
      percyCSS: `
        .allocation-row td { display: none; }
      `,
    });
  });

  test('Status Panel groups allocations when they get past a threshold, multiple statuses', async function (assert) {
    let groupTaskCount = 50;

    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 0.5,
        failed: 0.3,
        pending: 0.1,
        unknown: 0.1,
      },
      groupTaskCount,
      shallow: true,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    await setAllocationRowWidth(930);

    // With 50 allocs split across 4 statuses distributed as above, we can expect 25 running, 16 failed, 6 pending, and 4 remaining.
    // At the standard row width, each status will be ungrouped/grouped as follows:
    // 25 running: 9 ungrouped, 17 grouped
    // 15 failed: 5 ungrouped, 10 grouped
    // 5 pending: 0 ungrouped, 5 grouped
    // 5 unknown: 0 ungrouped, 5 grouped. Represented as "Unplaced"

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists({ count: 9 }, '9 running allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .exists(
        'Running allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .hasText(
        '+16',
        'Summary block has the correct number of grouped running allocs'
      );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-failed')
      .exists({ count: 5 }, '5 failed allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .exists(
        'Failed allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .hasText(
        '+10',
        'Summary block has the correct number of grouped failed allocs'
      );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-pending')
      .exists({ count: 0 }, '0 pending allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-pending')
      .exists(
        'pending allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-pending')
      .hasText(
        '5',
        'Summary block has the correct number of grouped pending allocs'
      );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-unplaced')
      .exists({ count: 0 }, '0 unplaced allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-unplaced')
      .exists(
        'Unplaced allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-unplaced')
      .hasText(
        '5',
        'Summary block has the correct number of grouped unplaced allocs'
      );
    await percySnapshot(
      'Status Panel groups allocations when they get past a threshold, multiple statuses (full width)',
      {
        percyCSS: `
          .allocation-row td { display: none; }
        `,
      }
    );

    // Simulate a window resize event; will recompute how many of each ought to be grouped.

    // At 1100px, only running and failed allocations have some ungrouped allocs
    // (a page of 1100px left the row 776px in the inherited layout)
    await setAllocationRowWidth(776);

    await percySnapshot(
      'Status Panel groups allocations when they get past a threshold, multiple statuses (1100px)',
      {
        percyCSS: `
          .allocation-row td { display: none; }
        `,
      }
    );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists({ count: 7 }, '7 running allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .exists(
        'Running allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .hasText(
        '+18',
        'Summary block has the correct number of grouped running allocs'
      );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-failed')
      .exists({ count: 4 }, '4 failed allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .exists(
        'Failed allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .hasText(
        '+11',
        'Summary block has the correct number of grouped failed allocs'
      );

    // At 500px, only running allocations have some ungrouped allocs. The rest are all fully grouped.
    // (a page of 800px left the row 500px in the inherited layout)
    await setAllocationRowWidth(500);

    await percySnapshot(
      'Status Panel groups allocations when they get past a threshold, multiple statuses (500px)',
      {
        percyCSS: `
          .allocation-row td { display: none; }
        `,
      }
    );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-running')
      .exists({ count: 4 }, '4 running allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .exists(
        'Running allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-running')
      .hasText(
        '+21',
        'Summary block has the correct number of grouped running allocs'
      );

    assert
      .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-failed')
      .doesNotExist('no failed allocations are represented ungrouped');
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .exists(
        'Failed allocations are numerous enough that a summary block exists'
      );
    assert
      .dom('.norca-alloc.norca-alloc-rest.norca-alloc-failed')
      .hasText(
        '15',
        'Summary block has the correct number of grouped failed allocs'
      );
  });

  test('Restarted/Rescheduled/Failed numbers reflected correctly', async function (assert) {
    this.store = this.owner.lookup('service:store');

    let groupTaskCount = 10;

    let job = server.create('job', {
      status: 'running',
      datacenters: ['*'],
      type: 'service',
      resourceSpec: ['M: 256, C: 500'], // a single group
      createAllocations: true,
      allocStatusDistribution: {
        running: 0.5,
        failed: 0.5,
        unknown: 0,
        lost: 0,
      },
      groupTaskCount,
      activeDeployment: true,
      shallow: true,
      version: 0,
    });

    let state = server.create('task-state');
    state.events = server.schema.taskEvents.where({ taskStateId: state.id });
    server.schema.allocations.where({ jobId: job.id }).update({
      taskStateIds: [state.id],
      jobVersion: 0,
    });

    await visit(`/jobs/${job.id}`);
    assert.dom('.norca-job-status').exists();
    assert
      .dom('.norca-failed-or-lost-links > span')
      .exists({ count: 2 }, 'Restarted and Rescheduled cells are both present');
    // await this.pauseTest();
    let rescheduledCell = [...findAll('.norca-failed-or-lost-links > span')][0];
    let restartedCell = [...findAll('.norca-failed-or-lost-links > span')][1];

    // Check that the title in each cell has the right text
    assert.dom(rescheduledCell).hasText('0 Rescheduled');
    assert.dom(restartedCell).hasText('0 Restarted');

    // Check that both values are zero and non-links
    assert
      .dom(rescheduledCell.querySelector('a'))
      .doesNotExist('Rescheduled cell is not a link');
    assert
      .dom(rescheduledCell)
      .hasText('0 Rescheduled', 'Rescheduled cell has zero value');
    assert
      .dom(restartedCell.querySelector('a'))
      .doesNotExist('Restarted cell is not a link');
    assert
      .dom(restartedCell)
      .hasText('0 Restarted', 'Restarted cell has zero value');

    // A wild event appears! Change a recent task event to type "Restarting" in a task state:
    this.store
      .peekAll('job')
      .objectAt(0)
      .get('allocations')
      .objectAt(0)
      .get('states')
      .objectAt(0)
      .get('events')
      .objectAt(0)
      .set('type', 'Restarting');

    await settled();

    assert
      .dom(restartedCell)
      .hasText(
        '1 Restarted',
        'Restarted cell updates when a task event with type "Restarting" is added'
      );

    this.store
      .peekAll('job')
      .objectAt(0)
      .get('allocations')
      .objectAt(1)
      .get('states')
      .objectAt(0)
      .get('events')
      .objectAt(0)
      .set('type', 'Restarting');

    await settled();

    // Trigger a reschedule! Set up a desiredTransition object with a Reschedule property on one of the allocations.
    assert
      .dom(restartedCell)
      .hasText(
        '2 Restarted',
        'Restarted cell updates when a second task event with type "Restarting" is added'
      );

    this.store
      .peekAll('job')
      .objectAt(0)
      .get('allocations')
      .objectAt(0)
      .get('followUpEvaluation')
      .set('content', { 'test-key': 'not-empty' });

    await settled();

    assert
      .dom(rescheduledCell)
      .hasText(
        '1 Rescheduled',
        'Rescheduled cell updates when desiredTransition is set'
      );
    assert
      .dom(rescheduledCell.querySelector('a'))
      .exists('Rescheduled cell with a non-zero number is now a link');
  });

  module('deployment history', function () {
    test('Deployment history can be searched', async function (assert) {
      faker.seed(1);

      let groupTaskCount = 10;

      let job = server.create('job', {
        status: 'running',
        datacenters: ['*'],
        type: 'service',
        resourceSpec: ['M: 256, C: 500'], // a single group
        createAllocations: true,
        allocStatusDistribution: {
          running: 1,
          failed: 0,
          unknown: 0,
          lost: 0,
        },
        groupTaskCount,
        shallow: true,
        activeDeployment: true,
        version: 0,
      });

      let state = server.create('task-state');
      state.events = server.schema.taskEvents.where({ taskStateId: state.id });

      server.schema.allocations.where({ jobId: job.id }).update({
        taskStateIds: [state.id],
        jobVersion: 0,
      });

      await visit(`/jobs/${job.id}`);
      assert.dom('.norca-job-status').exists();

      const serverEvents = server.schema.taskEvents.where({
        taskStateId: state.id,
      });
      const shownEvents = findAll('.norca-history-item');
      const jobAllocations = server.db.allocations.where({ jobId: job.id });
      assert.equal(
        shownEvents.length,
        serverEvents.length * jobAllocations.length,
        'All events are shown'
      );

      await fillIn(
        '[data-test-history-search]',
        serverEvents.models[0].message
      );
      assert.equal(
        findAll('.norca-history-item').length,
        jobAllocations.length,
        'Only events matching the search are shown'
      );

      await fillIn('[data-test-history-search]', 'foo bar baz');
      assert
        .dom('[data-test-history-search-no-match]')
        .exists('No match message is shown');
    });
  });

  module('Batch jobs', function () {
    test('Batch jobs have a valid Completed status', async function (assert) {
      this.store = this.owner.lookup('service:store');

      let batchJob = server.create('job', {
        status: 'running',
        datacenters: ['*'],
        type: 'batch',
        createAllocations: true,
        allocStatusDistribution: {
          running: 0.5,
          failed: 0.3,
          unknown: 0,
          lost: 0,
          complete: 0.2,
        },
        groupsCount: 1,
        groupTaskCount: 10,
        noActiveDeployment: true,
        shallow: true,
        version: 1,
      });

      let serviceJob = server.create('job', {
        status: 'running',
        datacenters: ['*'],
        type: 'service',
        createAllocations: true,
        allocStatusDistribution: {
          running: 0.5,
          failed: 0.3,
          unknown: 0,
          lost: 0,
          complete: 0.2,
        },
        groupsCount: 1,
        groupTaskCount: 10,
        noActiveDeployment: true,
        shallow: true,
        version: 1,
      });

      // Batch job should have 5 running, 3 failed, 2 completed
      await visit(`/jobs/${batchJob.id}`);
      assert.dom('.norca-job-status').exists();
      assert
        .dom('.norca-running-allocs-title')
        .hasText(
          '5/8 Remaining Allocations Running',
          'Completed allocations do not count toward the Remaining denominator'
        );
      assert
        .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-complete')
        .exists(
          { count: 2 },
          `2 complete allocations are represented in the status panel`
        );

      // Service job should have 5 running, 3 failed, 2 unplaced

      await visit(`/jobs/${serviceJob.id}`);
      assert.dom('.norca-job-status').exists();
      assert
        .dom('.norca-running-allocs-title')
        .hasText('5/10 Allocations Running');
      assert
        .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-complete')
        .doesNotExist(
          'For a service job, no copmlete allocations are represented in the status panel'
        );
      assert
        .dom('.norca-alloc-blocks .norca-alloc.norca-alloc-unplaced')
        .exists(
          { count: 2 },
          `2 unplaced allocations are represented in the status panel`
        );
    });
  });

  module('System jobs', function () {
    test('System jobs show restarted but not rescheduled allocs', async function (assert) {
      this.store = this.owner.lookup('service:store');

      let job = server.create('job', {
        status: 'running',
        datacenters: ['*'],
        type: 'system',
        createAllocations: true,
        allocStatusDistribution: {
          running: 0.5,
          failed: 0.5,
          unknown: 0,
          lost: 0,
        },
        noActiveDeployment: true,
        shallow: true,
        version: 0,
      });

      let state = server.create('task-state');
      state.events = server.schema.taskEvents.where({ taskStateId: state.id });
      server.schema.allocations.where({ jobId: job.id }).update({
        taskStateIds: [state.id],
        jobVersion: 0,
      });

      await visit(`/jobs/${job.id}`);
      assert.dom('.norca-job-status').exists();
      assert.dom('.norca-failed-or-lost').exists({ count: 1 });
      assert.dom('.norca-failed-or-lost h4').hasText('Replaced Allocations');
      assert
        .dom('.norca-failed-or-lost-links > span')
        .hasText('0 Restarted', 'Restarted cell at zero by default');

      // A wild event appears! Change a recent task event to type "Restarting" in a task state:
      this.store
        .peekAll('job')
        .objectAt(0)
        .get('allocations')
        .objectAt(0)
        .get('states')
        .objectAt(0)
        .get('events')
        .objectAt(0)
        .set('type', 'Restarting');

      await settled();

      assert
        .dom('.norca-failed-or-lost-links > span')
        .hasText(
          '1 Restarted',
          'Restarted cell updates when a task event with type "Restarting" is added'
        );
    });

    test('System jobs do not have a sense of Desired/Total allocs', async function (assert) {
      this.store = this.owner.lookup('service:store');

      server.db.nodes.remove();

      server.createList('node', 3, {
        status: 'ready',
        drain: false,
        schedulingEligibility: 'eligible',
      });

      let job = server.create('job', {
        status: 'running',
        datacenters: ['*'],
        type: 'system',
        createAllocations: false,
        noActiveDeployment: true,
        shallow: true,
        version: 0,
      });

      // Create an allocation on this job for each node
      server.schema.nodes.all().models.forEach((node) => {
        server.create('allocation', {
          jobId: job.id,
          jobVersion: 0,
          clientStatus: 'running',
          nodeId: node.id,
        });
      });

      await visit(`/jobs/${job.id}`);
      let storedJob = await this.store.find(
        'job',
        JSON.stringify([job.id, 'default'])
      );
      // Weird Mirage thing: job summary factory is disconnected from its job and therefore allocations.
      // So we manually create the number here.
      let summary = await storedJob.get('summary');
      summary
        .get('taskGroupSummaries')
        .objectAt(0)
        .set(
          'runningAllocs',
          server.schema.allocations.where({
            jobId: job.id,
            clientStatus: 'running',
          }).length
        );

      await settled();

      assert.dom('.norca-job-status').exists();
      assert.dom('.norca-running-allocs-title').hasText(
        `${
          server.schema.allocations.where({
            jobId: job.id,
            clientStatus: 'running',
          }).length
        } Allocations Running`
      );

      // Let's bring another node online!
      let newNode = server.create('node', {
        status: 'ready',
        drain: false,
        schedulingEligibility: 'eligible',
      });

      // Let's expect our scheduler to have therefore added an alloc to it
      server.create('allocation', {
        jobId: job.id,
        jobVersion: 0,
        clientStatus: 'running',
        nodeId: newNode.id,
      });

      summary
        .get('taskGroupSummaries')
        .objectAt(0)
        .set(
          'runningAllocs',
          server.schema.allocations.where({
            jobId: job.id,
            clientStatus: 'running',
          }).length
        );

      await settled();

      assert
        .dom('.norca-running-allocs-title')
        .hasText('4 Allocations Running');
    });
  });
});
