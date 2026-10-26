/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { htmlSafe } from '@ember/template';

/**
 * A horizontal stacked bar (allocation states, capacity).
 *
 *   <Ui::StackedBar @label="Allocations" @segments={{array
 *     (hash label="running" value=318 color="green")
 *     (hash label="pending" value=6 color="azure")
 *     (hash label="failed" value=3 color="red")}} />
 *
 * The accessible name lists every segment; segments with a value above zero
 * get a minimum width so that they stay visible.
 */
export default class UiStackedBar extends Component {
  get total() {
    return (this.args.segments || []).reduce(
      (sum, s) => sum + (s.value || 0),
      0
    );
  }

  get segments() {
    const total = this.total;
    if (!total) return [];
    return this.args.segments
      .filter((s) => s.value > 0)
      .map((s) => {
        const percent = Math.max((s.value / total) * 100, 1.5);
        return { ...s, style: htmlSafe(`width: ${percent.toFixed(2)}%`) };
      });
  }

  get description() {
    const parts = (this.args.segments || []).map(
      (s) => `${s.value} ${s.label}`
    );
    return `${this.args.label ? `${this.args.label}: ` : ''}${parts.join(
      ', '
    )}`;
  }
}
