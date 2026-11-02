/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class UiTooltip extends Component {
  get isShown() {
    return this.args.condition !== false && !!this.args.text;
  }

  // Long identifiers (no spaces) keep their start and end; prose is shown
  // in full.
  get text() {
    const text = String(this.args.text ?? '');
    if (this.args.full || this.args.wide || text.length < 30) return text;
    if (/\s/.test(text)) return text;
    return `${text.slice(0, 15).trim()}...${text.slice(-10).trim()}`;
  }
}
