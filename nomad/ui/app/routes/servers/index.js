/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

export default class ServersIndexRoute extends Route {
  setupController(controller) {
    super.setupController(...arguments);
    // The per-region summaries (leader, Raft peers, connection) load after
    // the server list is shown.
    if (!controller.isForbidden) controller.refreshRegions();
  }
}
