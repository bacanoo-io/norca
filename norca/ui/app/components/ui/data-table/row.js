/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

const INTERACTIVE =
  'a, button, input, select, textarea, label, [role="button"]';

export default class UiDataTableRow extends Component {
  get isClickable() {
    return typeof this.args.onClick === 'function';
  }

  @action
  open() {
    this.args.onClick?.(this.args.row);
  }

  // A click on a link or control inside the row does its own thing.
  @action
  click(event) {
    if (event.target.closest?.(INTERACTIVE)) return;
    this.open();
  }

  @action
  keydown(event) {
    if (event.key === 'Enter' && event.target === event.currentTarget) {
      event.preventDefault();
      this.open();
    }
  }

  @action
  toggle(event) {
    this.args.onToggle?.(this.args.row, event.target.checked);
  }

  @action
  stop(event) {
    event.stopPropagation();
  }
}
