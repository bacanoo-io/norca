/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { guidFor } from '@ember/object/internals';

export default class UiRadioGroup extends Component {
  get name() {
    return this.args.name || `radio-${guidFor(this)}`;
  }
}
