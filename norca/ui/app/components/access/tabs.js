/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

export default class AccessTabs extends Component {
  @service can;

  get tabs() {
    const tabs = [
      { id: 'overview', label: 'Overview', route: 'access-control.index' },
    ];
    if (this.can.can('list tokens')) {
      tabs.push({
        id: 'tokens',
        label: 'Tokens',
        route: 'access-control.tokens',
      });
    }
    if (this.can.can('list roles')) {
      tabs.push({ id: 'roles', label: 'Roles', route: 'access-control.roles' });
    }
    if (this.can.can('list policies')) {
      tabs.push({
        id: 'policies',
        label: 'Policies',
        route: 'access-control.policies',
      });
    }
    tabs.push({
      id: 'auth-methods',
      label: 'Auth methods',
      route: 'access-control.auth-methods',
    });
    return tabs.map((tab) => ({ ...tab, models: [] }));
  }
}
