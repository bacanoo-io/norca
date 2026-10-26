/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  create,
  collection,
  clickable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

export default create({
  visit: visitable('/servers/:name'),

  title: text('[data-test-title]'),
  serverStatus: text('[data-test-status]'),
  address: text('[data-test-address]'),
  datacenter: text('[data-test-datacenter]'),
  hasLeaderBadge: isPresent('[data-test-leader-badge]'),

  tags: collection('[data-test-server-tags] [data-test-attributes-section]', {
    name: text('[data-test-key]'),
    value: text('[data-test-value]'),
  }),

  error: {
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },
});
