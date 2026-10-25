/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

/**
 * A native select. @options are strings or { value, label } objects;
 * @onChange receives the selected value.
 *
 *   <Ui::Select @label="Region" @options={{this.regions}} @value={{this.region}} @onChange={{this.setRegion}} />
 *
 * @label is used as the accessible name when no Ui::Field label wraps it.
 */
export default class UiSelect extends Component {
  get options() {
    return (this.args.options || []).map((option) => {
      const o =
        typeof option === 'object' ? option : { value: option, label: option };
      return {
        ...o,
        value: String(o.value),
        isSelected: String(o.value) === String(this.args.value),
      };
    });
  }

  @action
  change(event) {
    this.args.onChange?.(event.target.value, event);
  }
}
