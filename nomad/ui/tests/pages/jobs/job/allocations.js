/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  clickable,
  create,
  collection,
  fillable,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

import allocations from 'nomad-ui/tests/pages/components/allocations';
import error from 'nomad-ui/tests/pages/components/error';
import { filterFacet } from 'nomad-ui/tests/pages/components/facet';

export default create({
  visit: visitable('/jobs/:id/allocations'),

  pageSize: 25,

  hasSearchBox: isPresent('[data-test-allocations-search]'),
  search: fillable('[data-test-allocations-search]'),

  ...allocations(),

  facets: {
    status: filterFacet('[data-test-allocation-status-facet]'),
    client: filterFacet('[data-test-allocation-client-facet]'),
    taskGroup: filterFacet('[data-test-allocation-task-group-facet]'),
  },

  isEmpty: isPresent('[data-test-empty-allocations-list]'),
  emptyState: {
    headline: text(
      '[data-test-empty-allocations-list] [data-test-empty-state-title]'
    ),
  },

  sortOptions: collection('[data-test-sort-by]', {
    id: attribute('data-test-sort-by'),
    sort: clickable(),
  }),

  sortBy(id) {
    return this.sortOptions.toArray().findBy('id', id).sort();
  },

  error: error(),
});
