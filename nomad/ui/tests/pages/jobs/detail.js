/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  create,
  collection,
  clickable,
  isPresent,
  property,
  text,
  visitable,
} from 'ember-cli-page-object';

import allocations from 'nomad-ui/tests/pages/components/allocations';
import taskGroups from 'nomad-ui/tests/pages/components/task-groups';
import twoStepButton from 'nomad-ui/tests/pages/components/two-step-button';
import jobClientStatusBar from 'nomad-ui/tests/pages/components/job-client-status-bar';

export default create({
  visit: visitable('/jobs/:id'),

  jobName: text('[data-test-job-name]'),

  tabs: collection('[data-test-tab]', {
    id: attribute('data-test-tab'),
    visit: clickable('a'),
  }),

  tabFor(id) {
    return this.tabs.toArray().findBy('id', id);
  },

  stop: twoStepButton('[data-test-stop]'),
  start: twoStepButton('[data-test-start]'),
  purge: twoStepButton('[data-test-purge]'),

  packTag: isPresent('[data-test-pack-tag]'),
  metaTable: isPresent('[data-test-meta]'),

  execButton: {
    scope: '[data-test-exec-button]',
    isDisabled: property('disabled'),
    hasTooltip: attribute('title'),
    tooltipText: attribute('title'),
  },

  incrementButton: {
    scope: '[data-test-scale-controls-increment]',
    isDisabled: property('disabled'),
  },

  dispatchButton: {
    scope: '[data-test-dispatch-button]',
    isDisabled: property('disabled'),
  },

  stats: collection('[data-test-job-stat]', {
    id: attribute('data-test-job-stat'),
    text: text(),
  }),

  statFor(id) {
    return this.stats.toArray().findBy('id', id);
  },

  packStats: collection('[data-test-pack-stat]', {
    id: attribute('data-test-pack-stat'),
    text: text(),
  }),

  packStatFor(id) {
    return this.packStats.toArray().findBy('id', id);
  },

  statusModes: {
    current: {
      scope: '[data-test-status-mode-current]',
      click: clickable(),
    },
    historical: {
      scope: '[data-test-status-mode-historical]',
      click: clickable(),
    },
  },

  childrenSummary: jobClientStatusBar(
    '[data-test-job-summary] [data-test-children-status-bar]'
  ),
  allocationsSummary: jobClientStatusBar(
    '[data-test-job-status-panel] [data-test-allocation-status-bar]'
  ),
  ...taskGroups(),
  ...allocations(),

  viewAllAllocations: text('[data-test-view-all-allocations]'),

  jobsHeader: {
    scope: '[data-test-ui-table] thead',
    hasSubmitTime: isPresent('[data-test-ui-table-header="submitTime"]'),
    hasNamespace: isPresent('[data-test-ui-table-header="namespace"]'),
    hasNodePool: isPresent('[data-test-ui-table-header="nodePool"]'),
    hasType: isPresent('[data-test-ui-table-header="type"]'),
    hasPriority: isPresent('[data-test-ui-table-header="priority"]'),
  },

  jobs: collection('[data-test-job-row]', {
    id: attribute('data-test-job-row'),
    name: text('[data-test-job-name]'),
    link: attribute('href', '[data-test-job-name] a'),
    namespace: text('[data-test-job-namespace]'),
    nodePool: text('[data-test-job-node-pool]'),
    submitTime: text('[data-test-job-submit-time]'),
    status: text('[data-test-job-status]'),
    type: text('[data-test-job-type]'),
    priority: text('[data-test-job-priority]'),
    taskGroups: text('[data-test-job-task-groups]'),

    clickRow: clickable(),
    clickName: clickable('[data-test-job-name] a'),
  }),

  error: {
    isPresent: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },

  recentAllocationsEmptyState: {
    headline: text(
      '[data-test-empty-recent-allocations] [data-test-empty-state-title]'
    ),
  },
});
