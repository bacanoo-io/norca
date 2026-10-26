/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { startBackup } from 'nomad-ui/utils/backups';

/**
 * The manual backup dialog: an optional note (stored as the `note` label)
 * and pinning. The backup runs detached on the leader of the region; its
 * record appears when it completes (Backup event).
 */
export default class BackupsBackupNow extends Component {
  @service token;
  @service notifications;

  @tracked note = '';
  @tracked pin = false;
  @tracked region = this.args.regions?.[0] || '';
  @tracked isRunning = false;
  @tracked error = null;

  get regionOptions() {
    return (this.args.regions || []).map((r) => ({
      value: r,
      label: r || 'Local region',
    }));
  }

  get hasRegionChoice() {
    return (this.args.regions || []).length > 1;
  }

  @action setNote(value) {
    this.note = value;
  }

  @action setPin(value) {
    this.pin = value;
  }

  @action setRegion(value) {
    this.region = value;
  }

  @action
  async submit(event) {
    event?.preventDefault();
    this.isRunning = true;
    this.error = null;
    try {
      const labels = this.note.trim() ? { note: this.note.trim() } : {};
      const { body } = await startBackup(this.token, this.region, {
        labels,
        pin: this.pin,
      });
      this.notifications.add({
        title: `Backup ${body?.ID || ''} started`,
        message:
          'It appears in the list when it is uploaded to every destination.',
        color: 'success',
      });
      this.args.onStarted?.(this.region, body?.ID);
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.isRunning = false;
    }
  }
}
