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
  value,
  visitable,
} from 'ember-cli-page-object';

import allocations from 'nomad-ui/tests/pages/components/allocations';
import error from 'nomad-ui/tests/pages/components/error';
import { nativePageSizeSelect } from 'nomad-ui/tests/pages/components/page-size-select';
import LifecycleChart from 'nomad-ui/tests/pages/components/lifecycle-chart';
import { filterFacet } from 'nomad-ui/tests/pages/components/facet';

export default create({
  pageSize: 25,

  visit: visitable('/jobs/:id/groups/:name'),

  search: fillable('[data-test-allocations-search]'),

  countStepper: {
    scope: '[data-test-task-group-count-stepper]',
    label: text('[data-test-stepper-label]'),
    input: {
      scope: '[data-test-stepper-input]',
      fill: fillable(),
      value: value(),
      isDisabled: attribute('disabled'),
    },
    decrement: {
      scope: '[data-test-stepper-decrement]',
      click: clickable(),
      isDisabled: attribute('disabled'),
    },
    increment: {
      scope: '[data-test-stepper-increment]',
      click: clickable(),
      isDisabled: attribute('disabled'),
    },
  },
  incrementButton: { scope: '[data-test-stepper-increment]' },

  tasksCount: text('[data-test-task-group-tasks]'),
  cpu: text('[data-test-task-group-cpu]'),
  mem: text('[data-test-task-group-mem]'),
  disk: text('[data-test-task-group-disk]'),

  ...allocations(),

  isEmpty: isPresent('[data-test-empty-allocations-list]'),

  facets: {
    status: filterFacet('[data-test-allocation-status-facet]'),
    client: filterFacet('[data-test-allocation-client-facet]'),
  },

  lifecycleChart: LifecycleChart,

  hasVolumes: isPresent('[data-test-volumes]'),
  volumes: collection('[data-test-volumes] [data-test-volume]', {
    name: text('[data-test-volume-name]'),
    type: text('[data-test-volume-type]'),
    source: text('[data-test-volume-source]'),
    permissions: text('[data-test-volume-permissions]'),
  }),

  hasScaleEvents: isPresent('[data-test-scale-events]'),
  scaleEvents: collection('[data-test-scale-events] [data-test-scale-event]', {
    error: isPresent('[data-test-error]'),
    time: text('[data-test-time]'),
    count: text('[data-test-count]'),
    countIcon: { scope: '[data-test-count-icon]' },
    message: text('[data-test-message]'),

    isToggleable: isPresent('[data-test-scale-event-toggle]'),
    toggle: clickable('[data-test-scale-event-toggle]'),
  }),

  scaleEventBodies: collection(
    '[data-test-scale-events] [data-test-scale-event-meta]',
    {
      meta: text(),
    }
  ),

  hasScalingTimeline: isPresent('[data-test-scaling-timeline]'),
  scalingAnnotations: collection(
    '[data-test-scaling-timeline] [data-test-annotation]'
  ),

  error: error(),

  emptyState: {
    headline: text(
      '[data-test-empty-allocations-list] [data-test-empty-state-title]'
    ),
  },

  pageSizeSelect: nativePageSizeSelect(),
});
