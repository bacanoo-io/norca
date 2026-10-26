/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { modifier } from 'ember-modifier';

export default class UiSidePanel extends Component {
  @tracked wide = false;

  titleId = `${guidFor(this)}-title`;

  @action
  close() {
    this.args.onClose?.();
  }

  @action
  toggleWide() {
    this.wide = !this.wide;
  }

  behavior = modifier((element) => {
    const keydown = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        this.close();
      }
    };
    const outside = (event) => {
      if (element.contains(event.target)) return;
      // Clicks on the controls that open a panel decide themselves.
      if (event.target.closest?.('[data-side-panel-trigger]')) return;
      this.close();
    };
    document.addEventListener('keydown', keydown);
    document.addEventListener('mousedown', outside, true);
    return () => {
      document.removeEventListener('keydown', keydown);
      document.removeEventListener('mousedown', outside, true);
    };
  });
}
