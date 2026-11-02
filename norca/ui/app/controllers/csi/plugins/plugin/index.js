/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { computed } from '@ember/object';

export default class IndexController extends Controller {
  @computed('model.controllers.@each.updateTime')
  get sortedControllers() {
    return this.model.controllers.sortBy('updateTime').reverse();
  }

  @computed('model.nodes.@each.updateTime')
  get sortedNodes() {
    return this.model.nodes.sortBy('updateTime').reverse();
  }
}
