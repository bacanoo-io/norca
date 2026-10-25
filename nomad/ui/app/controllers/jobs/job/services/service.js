/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';

export default class JobsJobServicesServiceController extends Controller {
  @service router;
  queryParams = ['level'];

  columns = [
    { key: 'allocation', label: 'Allocation', hideable: false },
    { key: 'client', label: 'Client' },
    { key: 'address', label: 'IP address and port' },
    { key: 'ready', label: 'Receives traffic' },
    { key: 'health', label: 'Health' },
    { key: 'checks', label: 'Checks' },
    { key: 'zone', label: 'Zone', hidden: true },
  ];

  @action
  gotoAllocation(allocation) {
    this.router.transitionTo('allocations.allocation', allocation.get('id'));
  }
}
