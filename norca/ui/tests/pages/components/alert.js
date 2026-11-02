/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { isPresent, clickable, text } from 'ember-cli-page-object';

// A Ui::Alert with a title (data-test-title) and a message
// (data-test-message), dismissed with its close button.
export default (scope) => ({
  scope,

  isPresent: isPresent(),
  dismiss: clickable('[data-test-alert-dismiss]'),
  title: text('[data-test-title]'),
  message: text('[data-test-message]'),
});
