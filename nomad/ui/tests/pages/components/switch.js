/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  clickable,
  isPresent,
  property,
  text,
} from 'ember-cli-page-object';

// A Ui::Switch (checkbox or switch); the scope is its label.
export default (scope) => ({
  scope,

  isPresent: isPresent(),
  isDisabled: property('disabled', '[data-test-switch-input]'),
  isActive: property('checked', '[data-test-switch-input]'),
  label: text(),
  title: attribute('title'),

  toggle: clickable('[data-test-switch-input]'),
});
