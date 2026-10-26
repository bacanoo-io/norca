/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

export default class BackupsBackupRoute extends Route {
  model({ backup_id }) {
    return { backupId: backup_id };
  }
}
