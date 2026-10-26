/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { clickable, collection, text, attribute } from 'ember-cli-page-object';
import { select } from '@ember/test-helpers';

// A filter of the unified lists (Ui::FilterDropdown): the options are in
// the dropdown menu next to the trigger and stay open while they are toggled.
export const filterFacet = (scope) => ({
  scope,

  toggle: clickable('[data-test-dropdown-trigger]'),
  isDisabled: attribute('disabled', '[data-test-dropdown-trigger]'),
  count: text('[data-test-dropdown-count]'),

  options: collection('[data-test-dropdown-option]', {
    label: text(),
    key: attribute('data-test-dropdown-option'),
    isSelected: attribute('aria-pressed'),
    toggle: clickable(),
  }),

  clear: clickable('[data-test-dropdown-clear]'),
});

// A single choice filter of the unified lists: a native select
// (Ui::Select). toggle() is a no-op kept for the facet test helpers.
export const nativeSingleFacet = (scope) => ({
  scope,

  async toggle() {},

  options: collection('option', {
    label: text(),
    key: attribute('value'),
    async select() {
      const parentScope = this.__parentTreeNode.scope;
      await select(parentScope, this.key);
    },
  }),
});
