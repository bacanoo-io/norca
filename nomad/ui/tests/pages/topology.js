/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  clickable,
  collection,
  create,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

import { filterFacet } from 'nomad-ui/tests/pages/components/facet';
import TopoViz from 'nomad-ui/tests/pages/components/topo-viz';

export default create({
  visit: visitable('/topology'),

  infoPanelTitle: text('[data-test-info-panel-title] [data-test-card-title]'),
  filteredNodesWarning: {
    scope: '[data-test-filtered-nodes-warning]',
    isPresent: isPresent(),
    title: text('[data-test-alert-title]'),
    message: text('[data-test-message]'),
    dismiss: clickable('[data-test-alert-dismiss]'),
  },

  viz: TopoViz('[data-test-topo-viz]'),

  facets: {
    nodePool: filterFacet('[data-test-node-pool-facet]'),
    datacenter: filterFacet('[data-test-datacenter-facet]'),
    class: filterFacet('[data-test-class-facet]'),
    state: filterFacet('[data-test-state-facet]'),
    version: filterFacet('[data-test-version-facet]'),
  },

  clusterInfoPanel: {
    scope: '[data-test-info-panel]',
    nodeCount: text('[data-test-node-count]'),
    allocCount: text('[data-test-alloc-count]'),
    nodePoolCount: text('[data-test-node-pool-count]'),

    memoryProgressValue: attribute(
      'aria-valuenow',
      '[data-test-memory-progress-bar]'
    ),
    memoryAbsoluteValue: text('[data-test-memory-absolute-value]'),
    cpuProgressValue: attribute(
      'aria-valuenow',
      '[data-test-cpu-progress-bar]'
    ),
    cpuAbsoluteValue: text('[data-test-cpu-absolute-value]'),
  },

  nodeInfoPanel: {
    scope: '[data-test-info-panel]',
    allocations: text('[data-test-allocaions]'),

    visitNode: clickable('[data-test-client-link]'),

    id: text('[data-test-client-link]'),
    name: text('[data-test-name]'),
    address: text('[data-test-address]'),
    status: text('[data-test-status]'),

    drainingLabel: text('[data-test-draining]'),
    drainingIsAccented: isPresent('[data-test-draining][data-test-accented]'),

    eligibleLabel: text('[data-test-eligible]'),
    eligibleIsAccented: isPresent('[data-test-eligible][data-test-accented]'),

    memoryProgressValue: attribute(
      'aria-valuenow',
      '[data-test-memory-progress-bar]'
    ),
    memoryAbsoluteValue: text('[data-test-memory-absolute-value]'),
    cpuProgressValue: attribute(
      'aria-valuenow',
      '[data-test-cpu-progress-bar]'
    ),
    cpuAbsoluteValue: text('[data-test-cpu-absolute-value]'),
  },

  allocInfoPanel: {
    scope: '[data-test-info-panel]',
    id: text('[data-test-id]'),
    visitAlloc: clickable('[data-test-id]'),

    siblingAllocs: text('[data-test-sibling-allocs]'),
    uniquePlacements: text('[data-test-unique-placements]'),

    job: text('[data-test-job]'),
    visitJob: clickable('[data-test-job]'),
    taskGroup: text('[data-test-task-group]'),

    client: text('[data-test-client]'),
    visitClient: clickable('[data-test-client]'),

    charts: collection('[data-test-primary-metric]', {
      areas: collection('[data-test-chart-area]', {
        taskName: attribute('data-test-task-name'),
      }),
    }),
  },
});
