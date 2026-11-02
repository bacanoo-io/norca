/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

/**
 * State of the backup pages that outlives a single page:
 *
 * - the age identity that decrypts the backups (browsing, comparison,
 *   restore). The server needs it for every content request because the
 *   backups are encrypted to a recipient whose private key the cluster does
 *   not hold. It is kept in memory only, for this browser tab: never in
 *   local storage, never in the URL.
 * - the items chosen for a selective restore while moving from the content
 *   browser to the restore wizard.
 */
export default class BackupsService extends Service {
  @tracked identity = '';

  /** backup ID -> [BackupRestoreItem] */
  @tracked drafts = {};

  get hasIdentity() {
    return !!this.identity.trim();
  }

  @action
  setIdentity(value) {
    this.identity = value || '';
  }

  @action
  forgetIdentity() {
    this.identity = '';
  }

  draftFor(backupId) {
    return this.drafts[backupId] || [];
  }

  setDraft(backupId, items) {
    this.drafts = { ...this.drafts, [backupId]: items };
  }

  clearDraft(backupId) {
    const drafts = { ...this.drafts };
    delete drafts[backupId];
    this.drafts = drafts;
  }
}
