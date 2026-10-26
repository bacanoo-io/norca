/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  create,
  clickable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';
import { _cancelTimers as cancelTimers, later } from '@ember/runloop';
import { select } from '@ember/test-helpers';

export default create({
  visit: visitable('/servers/:name/monitor'),

  logsArePresent: isPresent('[data-test-log-box]'),

  error: {
    isShown: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },

  async selectLogLevel(level) {
    later(cancelTimers, 500);
    await select('[data-test-level-switcher]', level.toLowerCase());
  },
});
