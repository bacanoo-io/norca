/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

export default class UiFilterDropdown extends Component {
  get selection() {
    return this.args.selection || [];
  }

  get options() {
    const selected = new Set(this.selection.map(String));
    return (this.args.options || []).map((option) => {
      const o =
        typeof option === 'object' ? option : { key: option, label: option };
      return {
        ...o,
        key: String(o.key),
        label: o.label ?? o.key,
        isSelected: selected.has(String(o.key)),
      };
    });
  }

  get isEmpty() {
    return !this.options.length && !this.selection.length;
  }

  get selectedCount() {
    return this.selection.length;
  }

  @action
  toggle(key, checked) {
    const rest = this.selection.filter((k) => String(k) !== key);
    this.args.onSelect?.(checked ? [...rest, key] : rest);
  }

  @action
  clear() {
    this.args.onSelect?.([]);
  }
}
