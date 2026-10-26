/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';

// The task group URLs of the previous UI (/jobs/:job/:group) lead to
// /jobs/:job/groups/:group.
export default class TaskGroupRedirectRoute extends Route {
  @service router;

  beforeModel(transition) {
    const job = this.modelFor('jobs.job');
    const { name } = transition.to.params;
    this.router.replaceWith('jobs.job.task-group', job, name, {
      queryParams: transition.to.queryParams,
    });
  }
}
