/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  clickable,
  collection,
  create,
  fillable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

import error from 'nomad-ui/tests/pages/components/error';
import { nativePageSizeSelect } from 'nomad-ui/tests/pages/components/page-size-select';

export default create({
  pageSize: 25,

  visit: visitable('/csi/plugins'),

  search: fillable('[data-test-plugins-search]'),

  plugins: collection('[data-test-plugin-row]', {
    id: text('[data-test-plugin-id]'),
    controllerHealth: text('[data-test-plugin-controller-health]'),
    nodeHealth: text('[data-test-plugin-node-health]'),
    provider: text('[data-test-plugin-provider]'),

    clickRow: clickable(),
    clickName: clickable('[data-test-plugin-id] a'),
  }),

  nextPage: clickable('[data-test-pagination-next]'),
  prevPage: clickable('[data-test-pagination-prev]'),

  isEmpty: isPresent('[data-test-empty-plugins-list]'),
  emptyState: {
    headline: text(
      '[data-test-empty-plugins-list] [data-test-empty-state-title]'
    ),
  },

  error: error(),
  pageSizeSelect: nativePageSizeSelect(),
});
