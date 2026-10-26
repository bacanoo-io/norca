/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { htmlSafe } from '@ember/template';

export default class UiDistribution extends Component {
  get data() {
    return (this.args.data || []).map((d) => ({ ...d, value: d.value || 0 }));
  }

  get total() {
    return this.data.reduce((sum, d) => sum + d.value, 0);
  }

  get segments() {
    const total = this.total;
    if (!total) return [];
    return this.data
      .filter((d) => d.value > 0)
      .map((d) => ({
        ...d,
        style: htmlSafe(
          `flex-grow: ${d.value}; min-width: 0.375rem; flex-basis: 0`
        ),
      }));
  }

  get legend() {
    return this.data.map((d) => ({
      ...d,
      isLink: !!d.route && d.value > 0,
      models: d.models || (d.model !== undefined ? [d.model] : []),
    }));
  }

  get description() {
    const parts = this.data.map((d) => `${d.value} ${d.label.toLowerCase()}`);
    return `${this.args.label ? `${this.args.label}: ` : ''}${parts.join(
      ', '
    )}`;
  }
}
