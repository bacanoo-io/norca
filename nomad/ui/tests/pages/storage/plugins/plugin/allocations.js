/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  clickable,
  create,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

import allocations from 'nomad-ui/tests/pages/components/allocations';
import { filterFacet } from 'nomad-ui/tests/pages/components/facet';
import { nativePageSizeSelect } from 'nomad-ui/tests/pages/components/page-size-select';

export default create({
  pageSize: 25,

  visit: visitable('/csi/plugins/:id/allocations'),

  nextPage: clickable('[data-test-pagination-next]'),
  prevPage: clickable('[data-test-pagination-prev]'),

  isEmpty: isPresent('[data-test-empty-list]'),
  emptyState: {
    headline: text('[data-test-empty-list] [data-test-empty-state-title]'),
  },

  ...allocations('[data-test-allocation]', 'allocations'),

  pageSizeSelect: nativePageSizeSelect(),

  facets: {
    health: filterFacet('[data-test-health-facet]'),
    type: filterFacet('[data-test-type-facet]'),
  },
});
