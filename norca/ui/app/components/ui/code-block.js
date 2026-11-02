/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class UiCodeBlock extends Component {
  get text() {
    const code = this.args.code;
    if (code === undefined || code === null) return '';
    if (typeof code === 'string') return code;
    return JSON.stringify(code, null, 2);
  }
}
