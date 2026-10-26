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
import { filterFacet } from 'nomad-ui/tests/pages/components/facet';

import clients from 'nomad-ui/tests/pages/components/clients';
import error from 'nomad-ui/tests/pages/components/error';

export default create({
  visit: visitable('/jobs/:id/clients'),
  pageSize: 25,

  hasSearchBox: isPresent('[data-test-clients-search]'),
  search: fillable('[data-test-clients-search]'),

  ...clients(),

  isEmpty: isPresent('[data-test-empty-clients-list]'),
  emptyState: {
    headline: text(
      '[data-test-empty-clients-list] [data-test-empty-state-title]'
    ),
  },

  sortOptions: collection('[data-test-sort-by]', {
    id: attribute('data-test-sort-by'),
    sort: clickable(),
  }),

  sortBy(id) {
    return this.sortOptions.toArray().findBy('id', id).sort();
  },

  facets: {
    jobStatus: filterFacet('[data-test-job-status-facet]'),
    datacenter: filterFacet('[data-test-datacenter-facet]'),
    clientClass: filterFacet('[data-test-client-class-facet]'),
  },

  error: error(),
});
