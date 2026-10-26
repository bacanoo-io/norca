/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

/**
 * The confirmation of a dangerous operation (UI plan, common UX patterns).
 * With @typedValue the confirm button stays disabled until the value is
 * typed exactly. Arguments: @title, @confirmLabel, @confirmIcon, @tone
 * (danger by default, or primary), @typedValue, @typedHelp, @onConfirm
 * (may return a promise), @onClose, @isRunning, @error, @size.
 */
export default class UiConfirm extends Component {
  @tracked typed = '';

  get canConfirm() {
    if (this.args.isRunning || this.args.disabled) return false;
    if (!this.args.typedValue) return true;
    return this.typed.trim() === String(this.args.typedValue);
  }

  @action
  setTyped(value) {
    this.typed = value;
  }

  @action
  confirm() {
    if (!this.canConfirm) return;
    return this.args.onConfirm?.(this.typed.trim());
  }

  @action
  cancel() {
    if (this.args.isRunning) return;
    this.args.onClose?.();
  }
}
