/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { guidFor } from '@ember/object/internals';

/**
 * A labelled form field. The block receives the control id and the
 * aria-describedby value to put on the control; the Ui::Input and Ui::Select
 * controls take them as @id and @describedBy.
 *
 *   <Ui::Field @label="Name" @help="Lowercase letters and dashes." @error={{this.nameError}} as |f|>
 *     <Ui::Input @id={{f.id}} @describedBy={{f.describedBy}} @value={{this.name}}
 *       @invalid={{f.invalid}} @onInput={{this.setName}} />
 *   </Ui::Field>
 */
export default class UiField extends Component {
  guid = guidFor(this);

  get id() {
    return this.args.id || `${this.guid}-control`;
  }

  get helpId() {
    return `${this.guid}-help`;
  }

  get errorId() {
    return `${this.guid}-error`;
  }

  get describedBy() {
    const ids = [];
    if (this.args.help) ids.push(this.helpId);
    if (this.args.error) ids.push(this.errorId);
    return ids.join(' ') || undefined;
  }
}
