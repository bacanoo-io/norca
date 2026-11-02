/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { modifier } from 'ember-modifier';

/**
 * The unified confirmation of dangerous operations (UI plan, common UX
 * patterns). By default a two-step button: the first click asks inline,
 * the second confirms. With @typedConfirmation the operation is confirmed
 * in a dialog by typing the given text (e.g. the name of the job).
 *
 *   <Ui::ConfirmButton
 *     @label="Stop job" @message="The allocations of the job are stopped."
 *     @confirmLabel="Yes, stop job" @onConfirm={{perform this.stopJob}}
 *     @isRunning={{this.stopJob.isRunning}} />
 *
 * @variant: danger (default), primary, warning. A click outside cancels the
 * inline prompt, Escape cancels both.
 */
export default class UiConfirmButton extends Component {
  @tracked state = 'idle';
  @tracked typed = '';

  formId = `${guidFor(this)}-form`;

  get isTyped() {
    return !!this.args.typedConfirmation;
  }

  get typedMatches() {
    return this.typed.trim() === String(this.args.typedConfirmation);
  }

  @action
  prompt() {
    this.typed = '';
    this.state = this.isTyped ? 'typed' : 'prompt';
    this.args.onPrompt?.();
  }

  @action
  cancel() {
    if (this.args.isRunning) return;
    this.state = 'idle';
    this.args.onCancel?.();
  }

  @action
  setTyped(value) {
    this.typed = value;
  }

  @action
  async confirm() {
    try {
      await this.args.onConfirm?.();
    } finally {
      if (!this.isDestroying && !this.isDestroyed) this.state = 'idle';
    }
  }

  @action
  submitTyped(event) {
    event.preventDefault();
    if (this.typedMatches) this.confirm();
  }

  // A click outside or Escape cancels the inline prompt.
  outside = modifier((element) => {
    const click = (event) => {
      if (this.state !== 'prompt' || this.args.isRunning) return;
      if (!element.contains(event.target)) this.cancel();
    };
    const keydown = (event) => {
      if (this.state === 'prompt' && event.key === 'Escape') this.cancel();
    };
    document.addEventListener('click', click, true);
    element.addEventListener('keydown', keydown);
    return () => {
      document.removeEventListener('click', click, true);
      element.removeEventListener('keydown', keydown);
    };
  });
}
