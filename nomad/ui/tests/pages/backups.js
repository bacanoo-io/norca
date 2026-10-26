/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  create,
  visitable,
  clickable,
  fillable,
  collection,
  text,
  isPresent,
} from 'ember-cli-page-object';

export default create({
  visit: visitable('/backups'),
  visitBackup: visitable('/backups/:id'),
  visitRestore: visitable('/backups/:id/restore'),
  visitRequest: visitable('/backups/restores/:id'),
  visitJobHistory: visitable('/backups/job-history'),

  backupNow: clickable('[data-test-backup-now]'),
  rows: collection('[data-test-backup-list] [data-test-ui-table-row]', {
    status: text('[data-test-backup-status]'),
    trigger: text('[data-test-backup-trigger]'),
  }),
  filter: fillable('[data-test-backup-filter]'),
  hasEmptyState: isPresent('[data-test-backups-empty]'),

  identity: {
    fill: fillable('[data-test-identity-input]'),
    submit: clickable('[data-test-identity-submit]'),
    isPresent: isPresent('[data-test-identity-form]'),
  },
});
