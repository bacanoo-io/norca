/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { debounce, cancel } from '@ember/runloop';

/**
 * A number input with decrement and increment buttons (a count). The value
 * shown changes at once; @onChange is called after @debounce milliseconds
 * (default 500) without a change. Escape in the input restores the value.
 *
 *   <Ui::Stepper @label="Count" @min={{1}} @max={{10}} @value={{@taskGroup.count}}
 *     @onChange={{this.scale}} @disabledReason={{this.whyNot}} />
 *
 * Adapted from the stepper input of the previous UI.
 */
export default class UiStepper extends Component {
  @tracked pending = null;

  id = `${guidFor(this)}-input`;
  timer = null;

  get min() {
    return this.args.min ?? 0;
  }

  get max() {
    return this.args.max ?? 10;
  }

  get value() {
    return this.pending ?? this.args.value ?? 0;
  }

  get isDisabled() {
    return !!this.args.disabled;
  }

  setCount(value) {
    this.pending = value;
    cancel(this.timer);
    this.timer = debounce(this, this.send, value, this.args.debounce ?? 500);
  }

  send(value) {
    if (this.isDestroying || this.isDestroyed) return;
    Promise.resolve(this.args.onChange?.(value)).finally(() => {
      if (!this.isDestroying && !this.isDestroyed) this.pending = null;
    });
  }

  @action
  increment() {
    if (this.value < this.max) this.setCount(this.value + 1);
  }

  @action
  decrement() {
    if (this.value > this.min) this.setCount(this.value - 1);
  }

  @action
  change(event) {
    if (event.target.value === '') {
      event.target.value = this.value;
      return;
    }
    const value = Math.floor(
      Math.min(this.max, Math.max(this.min, Number(event.target.value)))
    );
    event.target.value = value;
    this.setCount(value);
  }

  @action
  keyup(event) {
    if (event.key === 'Escape' || event.keyCode === 27) {
      event.target.value = this.value;
    }
  }

  @action
  select(event) {
    event.target.select();
  }

  willDestroy() {
    super.willDestroy(...arguments);
    cancel(this.timer);
  }
}
