/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { action, computed } from '@ember/object';
import legacyTransitionTo from 'nomad-ui/utils/legacy-transition';

export default class IndexController extends Controller {
  @computed('model.controllers.@each.updateTime')
  get sortedControllers() {
    return this.model.controllers.sortBy('updateTime').reverse();
  }

  @computed('model.nodes.@each.updateTime')
  get sortedNodes() {
    return this.model.nodes.sortBy('updateTime').reverse();
  }

  @action
  gotoAllocation(allocation) {
    legacyTransitionTo(this, 'allocations.allocation', allocation.id);
  }
}
