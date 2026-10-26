/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  collection,
  clickable,
  hasClass,
  isPresent,
  text,
  triggerable,
} from 'ember-cli-page-object';

const allocationRect = {
  select: clickable(),
  hover: triggerable('mouseenter'),
  mouseleave: triggerable('mouseleave'),
  width: attribute('width', '> rect'),
  height: attribute('height', '> rect'),
  isActive: hasClass('norca-topo-active'),
  isSelected: hasClass('norca-topo-selected'),
  running: hasClass('norca-topo-running'),
  failed: hasClass('norca-topo-failed'),
  pending: hasClass('norca-topo-pending'),
};

export default (scope) => ({
  scope,

  label: text('[data-test-label]'),
  labelIsPresent: isPresent('[data-test-label]'),
  statusIcon: attribute('data-test-icon', '[data-test-status-icon] svg'),
  statusIconLabel: attribute('aria-label', '[data-test-status-icon]'),

  selectNode: clickable('[data-test-node-background]'),
  nodeIsInteractive: hasClass(
    'norca-topo-interactive',
    '[data-test-node-background]'
  ),
  nodeIsSelected: hasClass(
    'norca-topo-selected',
    '[data-test-node-background]'
  ),

  memoryRects: collection('[data-test-memory-rect]', {
    ...allocationRect,
    id: attribute('data-test-memory-rect'),
  }),
  cpuRects: collection('[data-test-cpu-rect]', {
    ...allocationRect,
    id: attribute('data-test-cpu-rect'),
  }),

  mouseout: triggerable('mouseout', '[data-test-topo-node-svg]'),

  emptyMessage: text('[data-test-empty-message]'),
  isEmpty: hasClass('norca-topo-node-empty'),
});
