/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { select } from '@ember/test-helpers';
import {
  clickable,
  collection,
  isPresent,
  text,
  value,
} from 'ember-cli-page-object';

// The page size of the unified lists (Ui::Pagination): a native select.
export const nativePageSizeSelect = () => ({
  isPresent: isPresent('[data-test-page-size-select]'),
  selectedOption: value('[data-test-page-size-select]'),
  async choose(size) {
    await select('[data-test-page-size-select]', String(size));
  },
});

export default () => ({
  isPresent: isPresent(
    '[data-test-page-size-select-parent] .ember-power-select-trigger'
  ),
  open: clickable(
    '[data-test-page-size-select-parent] .ember-power-select-trigger'
  ),
  selectedOption: text(
    '[data-test-page-size-select-parent] .ember-power-select-selected-item'
  ),
  options: collection('.ember-power-select-option', {
    testContainer: '#ember-testing',
    resetScope: true,
    label: text(),
  }),
});
