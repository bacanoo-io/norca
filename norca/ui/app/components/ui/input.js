/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

/**
 * A text input (or textarea with @multiline). One-way value: @onInput
 * receives the new string.
 *
 *   <Ui::Input @value={{this.query}} @onInput={{this.setQuery}} @placeholder="Filter" @icon="search" />
 */
export default class UiInput extends Component {
  get type() {
    return this.args.type || 'text';
  }

  @action
  input(event) {
    this.args.onInput?.(event.target.value, event);
  }
}
