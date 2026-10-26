/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

// An option of Ui::RadioGroup: @value is the option, @groupValue (curried
// by the group) the selected value.
export default class UiRadioGroupOption extends Component {
  get groupValue() {
    return this.args.groupValue;
  }

  @action
  change(event) {
    if (event.target.checked) this.args.onChange?.(this.args.value, event);
  }
}
