/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';

/**
 * A modal dialog (Tabler markup, Ember behavior: no Bootstrap JavaScript).
 * Focus stays inside while open, Escape and the backdrop close it, focus
 * returns to the opener afterwards.
 *
 *   {{#if this.isOpen}}
 *     <Ui::Modal @title="Stop job?" @onClose={{this.close}}>
 *       <:default>The job's allocations are stopped.</:default>
 *       <:footer>
 *         <Ui::Button {{on "click" this.close}}>Cancel</Ui::Button>
 *         <Ui::Button @variant="danger" {{on "click" this.stop}}>Stop</Ui::Button>
 *       </:footer>
 *     </Ui::Modal>
 *   {{/if}}
 *
 * @size: sm, lg, xl. @tone: danger renders the status bar of dangerous
 * confirmations.
 */
export default class UiModal extends Component {
  titleId = `${guidFor(this)}-title`;

  get sizeClass() {
    return this.args.size ? `modal-${this.args.size}` : '';
  }

  @action
  close() {
    this.args.onClose?.();
  }

  @action
  backdropClick(event) {
    if (
      event.target === event.currentTarget &&
      this.args.dismissible !== false
    ) {
      this.close();
    }
  }
}
