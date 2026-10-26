/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import legacyTransitionTo from 'nomad-ui/utils/legacy-transition';

export default class DispatchRoute extends Route {
  @service can;

  beforeModel() {
    const job = this.modelFor('jobs.job');
    const namespace = job.namespace.get('name');
    if (this.can.cannot('dispatch job', null, { namespace })) {
      legacyTransitionTo(this, 'jobs.job');
    }
  }

  model() {
    const job = this.modelFor('jobs.job');
    if (!job) return legacyTransitionTo(this, 'jobs.job');
    return job;
  }
}
