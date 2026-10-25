/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

export default class UiCheckbox extends Component {
  @action
  change(event) {
    this.args.onChange?.(event.target.checked, event);
  }
}
