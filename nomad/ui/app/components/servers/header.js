/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

// The header of the server pages: the position, the name, the leader badge
// and the overview/monitor tabs.
export default class ServersHeader extends Component {
  get breadcrumbs() {
    const server = this.args.server;
    return [
      { label: 'Servers', route: 'servers.index', id: 'servers.index' },
      {
        label: server.name,
        route: 'servers.server',
        model: server.id,
        id: 'servers.server',
      },
    ];
  }

  get tabs() {
    const models = [this.args.server.id];
    return [
      {
        id: 'overview',
        label: 'Overview',
        route: 'servers.server.index',
        models,
      },
      {
        id: 'monitor',
        label: 'Monitor',
        route: 'servers.server.monitor',
        models,
      },
    ];
  }
}
