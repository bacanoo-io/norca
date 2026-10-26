/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { htmlSafe } from '@ember/template';

export default class UiProgress extends Component {
  get max() {
    return Number(this.args.max ?? 100) || 0;
  }

  get value() {
    const value = Number(this.args.value) || 0;
    return Math.min(Math.max(value, 0), this.max);
  }

  get percent() {
    return this.max ? (this.value / this.max) * 100 : 0;
  }

  get valueText() {
    return this.args.valueText ?? `${this.value} of ${this.max}`;
  }

  get style() {
    return htmlSafe(`width: ${this.percent.toFixed(2)}%`);
  }
}
