/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  create,
  collection,
  clickable,
  text,
  visitable,
} from 'ember-cli-page-object';

export default create({
  pageSize: 8,

  visit: visitable('/servers'),
  servers: collection('[data-test-server-agent-row]', {
    name: text('[data-test-server-name]'),
    status: text('[data-test-server-status]'),
    leader: text('[data-test-server-is-leader]'),
    address: text('[data-test-server-address]'),
    serfPort: text('[data-test-server-port]'),
    datacenter: text('[data-test-server-datacenter]'),
    version: text('[data-test-server-version]'),
    region: text('[data-test-server-region]'),
    raft: text('[data-test-server-raft]'),

    clickRow: clickable(),
    clickName: clickable('[data-test-server-name] a'),
  }),

  regions: collection('[data-test-regions] [data-test-ui-table-row]', {
    name: text('[data-test-region-name]'),
    connection: text('[data-test-region-connection]'),
    leader: text('[data-test-region-leader]'),
    servers: text('[data-test-region-servers]'),
    raft: text('[data-test-region-raft]'),
  }),

  error: {
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },
});
