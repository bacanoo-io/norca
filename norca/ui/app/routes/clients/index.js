/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { collect } from '@ember/object/computed';
import { watchAll } from 'nomad-ui/utils/properties/watch';
import WithWatchers from 'nomad-ui/mixins/with-watchers';
import { inject as service } from '@ember/service';

export default class IndexRoute extends Route.extend(WithWatchers) {
  @service store;
  @service nodeScaling;

  setupController(controller, model) {
    super.setupController(controller, model);
    // The node groups of the badges and the Node Group filter.
    this.nodeScaling.refresh(0);
  }

  startWatchers(controller) {
    controller.set('watcher', this.watch.perform());
  }

  @watchAll('node') watch;
  @collect('watch') watchers;
}
