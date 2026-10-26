/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

export default class AllocationsServicePanel extends Component {
  @service system;

  checkColumns = [
    { key: 'name', label: 'Check name' },
    { key: 'status', label: 'Status' },
    { key: 'output', label: 'Output' },
  ];

  get address() {
    const port = this.args.allocation?.allocatedResources?.ports?.findBy(
      'label',
      this.args.service.portLabel
    );
    return port ? `${port.hostIp}:${port.value}` : null;
  }

  get aggregateStatus() {
    if (this.args.allocation?.clientStatus !== 'running') return 'Unknown';
    return this.checks.any((check) => check.Status === 'failure')
      ? 'Unhealthy'
      : 'Healthy';
  }

  get consulRedirectLink() {
    return this.system.agent?.get('config')?.UI?.Consul?.BaseUIURL;
  }

  // A check may update less often than the UI polls, so the service keeps
  // duplicates: only the latest result of each check is listed.
  get checks() {
    if (!this.args.service || !this.args.allocation) return [];
    const allocID = this.args.allocation.id;
    return (this.args.service.healthChecks || [])
      .filterBy('Alloc', allocID)
      .sortBy('Timestamp')
      .reverse()
      .uniqBy('Check')
      .sortBy('Check');
  }
}
