/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

export default class BackupsRequestRoute extends Route {
  model({ restore_id }) {
    return { restoreId: restore_id };
  }
}
