/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { htmlSafe } from '@ember/template';
import { assert } from '@ember/debug';
import ICONS from 'nomad-ui/utils/icons/tabler';

const SIZES = { sm: 'icon-sm', md: '', lg: 'icon-lg' };

function escapeAttr(value) {
  return String(value)
    .replace(/&/g, '&amp;')
    .replace(/"/g, '&quot;')
    .replace(/</g, '&lt;');
}

/**
 * A Tabler icon (outline style).
 *
 *   <Ui::Icon @name="bell" />
 *   <Ui::Icon @name="search" @size="sm" @label="Search" />
 *
 * Without @label the icon is decorative (aria-hidden). Icons are listed in
 * scripts/generate-icons.js.
 */
export default class UiIcon extends Component {
  get nodes() {
    const nodes = ICONS[this.args.name];
    assert(
      `Unknown icon "${this.args.name}"; add it to scripts/generate-icons.js`,
      nodes
    );
    return nodes || [];
  }

  get sizeClass() {
    return SIZES[this.args.size || 'md'] || '';
  }

  get content() {
    const markup = this.nodes
      .map(([tag, attrs]) => {
        const a = Object.entries(attrs)
          .filter(([key]) => key !== 'key')
          .map(([key, value]) => `${key}="${escapeAttr(value)}"`)
          .join(' ');
        return `<${tag} ${a}></${tag}>`;
      })
      .join('');
    return htmlSafe(markup);
  }
}
