/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { attribute, clickable, isPresent, text } from 'ember-cli-page-object';
import { getter } from 'ember-cli-page-object/macros';
import { findMany } from 'ember-cli-page-object/extend';

export default (scope) => ({
  scope,

  isPresent: isPresent(),

  idle: clickable('[data-test-idle-button]'),
  confirm: clickable('[data-test-confirm-button]'),
  cancel: clickable('[data-test-cancel-button]'),

  // TwoStepButton marks the confirm button as loading, Ui::ConfirmButton
  // the button shown while the action runs.
  isRunning: getter(function () {
    return [
      ...findMany(this, '[data-test-confirm-button]'),
      ...findMany(this, '[data-test-idle-button]'),
    ].some(
      (button) =>
        button.classList.contains('is-loading') ||
        button.classList.contains('btn-loading')
    );
  }),
  isDisabled: attribute('disabled', '[data-test-idle-button]'),

  cancelIsDisabled: attribute('disabled', '[data-test-cancel-button]'),
  confirmIsDisabled: attribute('disabled', '[data-test-confirm-button]'),

  idleText: text('[data-test-idle-button]'),
  cancelText: text('[data-test-cancel-button]'),
  confirmText: text('[data-test-confirm-button]'),
  confirmationMessage: text('[data-test-confirmation-message]'),
});
