/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { later, cancel } from '@ember/runloop';

export default class UiCopyButton extends Component {
  @tracked copied = false;
  @tracked failed = false;

  timer = null;

  @action
  async copy() {
    cancel(this.timer);
    try {
      await navigator.clipboard.writeText(String(this.args.text ?? ''));
      this.copied = true;
      this.failed = false;
    } catch (e) {
      this.failed = true;
      this.copied = false;
    }
    this.timer = later(
      this,
      () => {
        this.copied = false;
        this.failed = false;
      },
      2000
    );
  }

  willDestroy() {
    super.willDestroy(...arguments);
    cancel(this.timer);
  }
}
