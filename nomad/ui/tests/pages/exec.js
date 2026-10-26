/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  clickable,
  collection,
  create,
  isPresent,
  text,
  triggerable,
  visitable,
} from 'ember-cli-page-object';

export default create({
  visitJob: visitable('/exec/:job'),
  visitTaskGroup: visitable('/exec/:job/:task_group'),
  visitTask: visitable('/exec/:job/:task_group/:task_name'),

  header: {
    region: { scope: '[data-test-region]' },
    namespace: { scope: '[data-test-namespace]' },
    job: text('[data-test-job]'),
  },

  taskGroups: collection('[data-test-task-group]', {
    click: clickable('[data-test-task-group-name]'),
    name: text('[data-test-task-group-name]'),

    isLoading: isPresent('[data-test-task-group-name] [data-test-loading]'),

    chevron: {
      scope: '[data-test-task-group-name]',
      isDown: isPresent('[data-test-chevron="down"]'),
      isRight: isPresent('[data-test-chevron="right"]'),
    },

    tasks: collection('[data-test-task]', {
      name: text(),
      isActive: isPresent('[data-test-task-active]'),
    }),
  }),

  terminal: {
    scope: '.xterm-helper-textarea',
    pressEnter: triggerable('keydown', '', {
      eventProperties: { keyCode: 13 },
    }),
  },

  jobDead: {
    scope: '[data-test-exec-job-dead]',
    message: text('[data-test-exec-job-dead-message]'),
  },
});
