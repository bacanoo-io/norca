/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

export default class UiColumnPicker extends Component {
  @action
  toggle(column, checked) {
    this.args.onToggle?.(column, checked);
  }
}
