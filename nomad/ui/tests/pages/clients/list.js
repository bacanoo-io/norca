/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  create,
  collection,
  clickable,
  fillable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

import { filterFacet } from 'nomad-ui/tests/pages/components/facet';
import { nativePageSizeSelect } from 'nomad-ui/tests/pages/components/page-size-select';

export default create({
  pageSize: 25,

  visit: visitable('/clients'),

  search: fillable('[data-test-clients-search]'),

  sortOptions: collection('[data-test-sort-by]', {
    id: attribute('data-test-sort-by'),
    sort: clickable(),
  }),

  sortBy(id) {
    return this.sortOptions.toArray().findBy('id', id).sort();
  },

  nodes: collection('[data-test-client-node-row]', {
    id: text('[data-test-client-id]'),
    name: text('[data-test-client-name]'),

    compositeStatus: {
      scope: '[data-test-client-composite-status]',

      tooltip: attribute('data-norca-tooltip', '[data-test-tooltip]'),
      status: attribute(
        'data-test-composite-status',
        '[data-test-composite-status]'
      ),
    },

    address: text('[data-test-client-address]'),
    nodePool: text('[data-test-client-node-pool]'),
    datacenter: text('[data-test-client-datacenter]'),
    version: text('[data-test-client-version]'),
    allocations: text('[data-test-client-allocations]'),

    clickRow: clickable(),
    clickName: clickable('[data-test-client-name] a'),
  }),

  hasPagination: isPresent('[data-test-pagination]'),

  isEmpty: isPresent('[data-test-empty-clients-list]'),
  empty: {
    headline: text(
      '[data-test-empty-clients-list] [data-test-empty-state-title]'
    ),
  },

  pageSizeSelect: nativePageSizeSelect(),

  error: {
    isPresent: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
    seekHelp: clickable('[data-test-error-message] a'),
  },

  facets: {
    nodePools: filterFacet('[data-test-node-pool-facet]'),
    class: filterFacet('[data-test-class-facet]'),
    state: filterFacet('[data-test-state-facet]'),
    datacenter: filterFacet('[data-test-datacenter-facet]'),
    version: filterFacet('[data-test-version-facet]'),
    volume: filterFacet('[data-test-volume-facet]'),
  },
});
