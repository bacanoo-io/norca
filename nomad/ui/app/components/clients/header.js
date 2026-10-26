/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class ClientsHeader extends Component {
  get breadcrumbs() {
    const client = this.args.client;
    return [
      { label: 'Clients', route: 'clients.index', id: 'clients.index' },
      {
        label: client.shortId,
        route: 'clients.client',
        model: client.id,
        id: 'clients.client',
      },
    ];
  }
}
