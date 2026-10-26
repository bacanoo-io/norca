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
import { nativeSingleFacet } from 'nomad-ui/tests/pages/components/facet';
import { nativePageSizeSelect } from 'nomad-ui/tests/pages/components/page-size-select';

export default create({
  pageSize: 25,

  visit: visitable('/csi/volumes'),

  search: fillable('[data-test-volumes-search]'),

  volumes: collection('[data-test-volume-row]', {
    name: text('[data-test-volume-name]'),
    namespace: text('[data-test-volume-namespace]'),
    schedulable: text('[data-test-volume-schedulable]'),
    controllerHealth: text('[data-test-volume-controller-health]'),
    nodeHealth: text('[data-test-volume-node-health]'),
    provider: text('[data-test-volume-provider]'),
    allocations: text('[data-test-volume-allocations]'),

    hasNamespace: isPresent('[data-test-volume-namespace]'),
    clickRow: clickable(),
    clickName: clickable('[data-test-volume-name] a'),
  }),

  nextPage: clickable('[data-test-pagination-next]'),
  prevPage: clickable('[data-test-pagination-prev]'),

  isEmpty: isPresent('[data-test-empty-volumes-list]'),
  emptyState: {
    headline: text(
      '[data-test-empty-volumes-list] [data-test-empty-state-title]'
    ),
  },

  error: error(),
  pageSizeSelect: nativePageSizeSelect(),

  facets: {
    namespace: nativeSingleFacet('[data-test-namespace-facet]'),
  },
});
