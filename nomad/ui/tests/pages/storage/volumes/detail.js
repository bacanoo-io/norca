/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { create, isPresent, text, visitable } from 'ember-cli-page-object';

import allocations from 'nomad-ui/tests/pages/components/allocations';

export default create({
  visit: visitable('/csi/volumes/:id'),

  title: text('[data-test-title]'),

  health: text('[data-test-volume-health]'),
  provider: text('[data-test-volume-provider]'),
  externalId: text('[data-test-volume-external-id]'),
  hasNamespace: isPresent('[data-test-volume-namespace]'),
  namespace: text('[data-test-volume-namespace]'),

  // The two allocation tables (Allocations::Table) of the page.
  readTable: {
    scope: '[data-test-read-allocations]',
    ...allocations('[data-test-allocation]', 'readAllocations'),
  },
  writeTable: {
    scope: '[data-test-write-allocations]',
    ...allocations('[data-test-allocation]', 'writeAllocations'),
  },

  writeTableIsEmpty: isPresent('[data-test-empty-write-allocations]'),
  writeEmptyState: {
    headline: text(
      '[data-test-empty-write-allocations] [data-test-empty-state-title]'
    ),
  },

  readTableIsEmpty: isPresent('[data-test-empty-read-allocations]'),
  readEmptyState: {
    headline: text(
      '[data-test-empty-read-allocations] [data-test-empty-state-title]'
    ),
  },

  constraints: {
    accessMode: text('[data-test-access-mode]'),
    attachmentMode: text('[data-test-attachment-mode]'),
  },
});
