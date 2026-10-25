/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  clickable,
  collection,
  hasClass,
  text,
} from 'ember-cli-page-object';

export default {
  scope: '[data-test-lifecycle-chart]',

  title: text('[data-test-card-title]'),

  phases: collection('[data-test-lifecycle-phase]', {
    name: text('[data-test-name]'),

    isActive: hasClass('norca-lifecycle-active'),
  }),

  tasks: collection('[data-test-lifecycle-task]', {
    name: text('[data-test-name]'),
    lifecycle: text('[data-test-lifecycle]'),

    state: attribute('data-test-lifecycle-state'),
    isActive: hasClass('norca-lifecycle-active'),
    isFinished: hasClass('norca-lifecycle-finished'),

    isMain: hasClass('norca-lifecycle-main'),
    isPrestartEphemeral: hasClass('norca-lifecycle-prestart-ephemeral'),
    isPrestartSidecar: hasClass('norca-lifecycle-prestart-sidecar'),
    isPoststartEphemeral: hasClass('norca-lifecycle-poststart-ephemeral'),
    isPoststartSidecar: hasClass('norca-lifecycle-poststart-sidecar'),
    isPoststop: hasClass('norca-lifecycle-poststop'),

    visit: clickable('a'),
  }),
};
