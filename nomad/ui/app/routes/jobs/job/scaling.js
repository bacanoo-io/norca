/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { collect } from '@ember/object/computed';
import { resolve } from 'rsvp';
import { watchRecord } from 'nomad-ui/utils/properties/watch';
import WithWatchers from 'nomad-ui/mixins/with-watchers';

// The Scaling tab: the job with its task groups and its scale status
// (counts and scaling events), kept fresh with blocking queries.
export default class ScalingRoute extends Route.extend(WithWatchers) {
  model() {
    const job = this.modelFor('jobs.job');
    if (!job) return;
    const reload = job.get('isPartial') ? job.reload() : resolve();
    return reload.then(() => job.get('scaleState')).then(() => job);
  }

  startWatchers(controller, model) {
    if (model) {
      controller.set('watchers', {
        job: this.watchJob.perform(model),
        scale: this.watchScale.perform(model.get('scaleState')),
      });
    }
  }

  @watchRecord('job') watchJob;
  @watchRecord('job-scale') watchScale;

  @collect('watchJob', 'watchScale') watchers;
}
