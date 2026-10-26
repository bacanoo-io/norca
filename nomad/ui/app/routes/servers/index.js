/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

// The servers of every region (the members and the regions card) are
// listed whatever the region scope, so "All regions" stays on this page.
export default class ServersIndexRoute extends Route {
  buildRouteInfoMetadata() {
    return { spansRegions: true };
  }

  setupController(controller) {
    super.setupController(...arguments);
    // The per-region summaries (leader, Raft peers, connection) load after
    // the server list is shown.
    if (!controller.isForbidden) controller.refreshRegions();
  }
}
