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
  text,
  visitable,
} from 'ember-cli-page-object';

import twoStepButton from 'nomad-ui/tests/pages/components/two-step-button';

export default create({
  visit: visitable('/allocations/:id/:name'),

  title: {
    scope: '[data-test-title]',

    proxyTag: {
      scope: '[data-test-proxy-tag]',
    },
  },

  state: text('[data-test-title] [data-test-state]'),
  startedAt: text('[data-test-started-at]'),

  lifecycle: text('[data-test-task-details] [data-test-lifecycle]'),

  restart: twoStepButton('[data-test-restart]'),

  execButton: {
    scope: '[data-test-exec-button]',
  },

  resourceCharts: collection('[data-test-primary-metric]', {
    name: text('[data-test-primary-metric-title]'),
    color: attribute('data-test-current-value', '[data-test-current-value]'),
  }),

  resourceEmptyMessage: text(
    '[data-test-resource-error] [data-test-empty-state-title]'
  ),

  hasPrestartTasks: isPresent('[data-test-prestart-tasks]'),
  prestartTasks: collection('[data-test-prestart-task]', {
    name: text('[data-test-name]'),
    state: text('[data-test-state]'),
    lifecycle: text('[data-test-lifecycle]'),
    isBlocking: isPresent('.icon-is-warning'),
  }),

  hasVolumes: isPresent('[data-test-volumes]'),
  volumes: collection('[data-test-volumes] [data-test-ui-table-row]', {
    name: text('[data-test-volume-name]'),
    destination: text('[data-test-volume-destination]'),
    permissions: text('[data-test-volume-permissions]'),
    clientSource: text('[data-test-volume-client-source]'),
  }),

  events: collection('[data-test-task-events] [data-test-ui-table-row]', {
    time: text('[data-test-task-event-time]'),
    type: text('[data-test-task-event-type]'),
    message: text('[data-test-task-event-message]'),
  }),

  error: {
    isPresent: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },

  inlineError: {
    isShown: isPresent('[data-test-inline-error]'),
    title: text('[data-test-inline-error-title]'),
    message: text('[data-test-inline-error-body]'),
    dismiss: clickable('[data-test-inline-error] [data-test-alert-dismiss]'),
  },
});
