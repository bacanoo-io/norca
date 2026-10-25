/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { get } from '@ember/object';
import { inject as service } from '@ember/service';
import { isTesting } from '@ember/debug';
import { task, timeout } from 'ember-concurrency';

/**
 *   <Allocations::TaskStateRow @taskState={{state}} @r={{r}} />
 *
 * All tasks of an allocation share the allocation's stats tracker.
 */
export default class AllocationsTaskStateRow extends Component {
  @service('stats-trackers-registry') statsTrackersRegistry;

  @tracked statsError = false;

  enablePolling = !isTesting();

  constructor() {
    super(...arguments);
    if (this.args.taskState?.allocation) this.fetchStats.perform();
  }

  get stats() {
    const taskState = this.args.taskState;
    if (!taskState?.isRunning) return undefined;
    return this.statsTrackersRegistry.getTracker(taskState.allocation);
  }

  get taskStats() {
    const stats = this.stats;
    if (!stats) return null;
    const tasks = get(stats, 'tasks') || [];
    return tasks.find((t) => t.task === this.args.taskState.name) || null;
  }

  @task({ drop: true })
  *fetchStats() {
    do {
      if (this.stats) {
        try {
          yield this.stats.poll.linked().perform();
          this.statsError = false;
        } catch (error) {
          this.statsError = true;
        }
      }
      yield timeout(500);
    } while (this.enablePolling);
  }
}
