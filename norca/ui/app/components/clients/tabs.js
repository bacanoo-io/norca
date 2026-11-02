/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class ClientsTabs extends Component {
  get tabs() {
    const models = [this.args.client];
    return [
      {
        id: 'overview',
        label: 'Overview',
        route: 'clients.client.index',
        models,
      },
      {
        id: 'monitor',
        label: 'Monitor',
        route: 'clients.client.monitor',
        models,
      },
    ];
  }
}
