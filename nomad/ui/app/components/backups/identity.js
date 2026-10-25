/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';

export default class BackupsIdentity extends Component {
  @service backups;

  @tracked value = '';

  @action
  setValue(value) {
    this.value = value;
  }

  @action
  submit(event) {
    event.preventDefault();
    if (!this.value.trim()) return;
    this.backups.setIdentity(this.value.trim());
    this.value = '';
    this.args.onUnlock?.();
  }

  @action
  forget() {
    this.backups.forgetIdentity();
    this.args.onForget?.();
  }
}
