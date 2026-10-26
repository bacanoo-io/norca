/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';

export default class StoragePluginAllocations extends Component {
  @service router;

  get columns() {
    const sortable = !!this.args.onSort;
    return [
      { key: 'indicators', label: 'Indicators', srOnly: true },
      { key: 'id', label: 'ID' },
      { key: 'created', label: 'Created' },
      { key: 'updateTime', label: 'Modified', sortable },
      { key: 'healthy', label: 'Health', sortable },
      { key: 'client', label: 'Client' },
      { key: 'job', label: 'Job' },
      { key: 'version', label: 'Version' },
      { key: 'volume', label: 'Volumes' },
      { key: 'cpu', label: 'CPU' },
      { key: 'memory', label: 'Memory' },
    ];
  }

  @action
  gotoAllocation(pluginAllocation) {
    this.router.transitionTo(
      'allocations.allocation',
      pluginAllocation.allocID
    );
  }
}
