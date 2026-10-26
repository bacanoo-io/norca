/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { clickable, collection, text, attribute } from 'ember-cli-page-object';
import { select } from '@ember/test-helpers';
import { selectChoose } from 'ember-power-select/test-support';
import { clickTrigger } from 'ember-power-select/test-support/helpers';

export const multiFacet = (scope) => ({
  scope,

  toggle: clickable('[data-test-dropdown-trigger]'),

  options: collection('[data-test-dropdown-option]', {
    testContainer: '#ember-testing .ember-basic-dropdown-content',
    resetScope: true,
    label: text(),
    key: attribute('data-test-dropdown-option'),
    toggle: clickable('label'),
  }),
});

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

export const singleFacet = (scope) => ({
  scope,

  async toggle() {
    await clickTrigger(this.scope);
  },

  options: collection('.ember-power-select-option', {
    testContainer: '#ember-testing .ember-basic-dropdown-content',
    resetScope: true,
    label: text('[data-test-dropdown-option]'),
    key: attribute('data-test-dropdown-option', '[data-test-dropdown-option]'),
    async select() {
      // __parentTreeNode is clearly meant to be private in the page object API,
      // but it seems better to use that and keep the API symmetry across singleFacet
      // and multiFacet compared to moving select to the parent.
      const parentScope = this.__parentTreeNode.scope;
      await selectChoose(parentScope, this.label);
    },
  }),
});
