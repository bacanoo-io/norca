/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { select } from '@ember/test-helpers';
import { isPresent, value } from 'ember-cli-page-object';

// The page size of the unified lists (Ui::Pagination): a native select.
export const nativePageSizeSelect = () => ({
  isPresent: isPresent('[data-test-page-size-select]'),
  selectedOption: value('[data-test-page-size-select]'),
  async choose(size) {
    await select('[data-test-page-size-select]', String(size));
  },
});
