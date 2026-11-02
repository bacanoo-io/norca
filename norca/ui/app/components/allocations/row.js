/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { isTesting } from '@ember/debug';
import { task, timeout } from 'ember-concurrency';

/**
 * A row of Allocations::Table: the allocation's cells, its resource usage
 * (polled while it runs) and, with @showSubTasks, a row per task.
 */
export default class AllocationsRow extends Component {
  @service store;
  @service('stats-trackers-registry') statsTrackersRegistry;

  @tracked statsError = false;

  enablePolling = !isTesting();

  constructor() {
    super(...arguments);
    this.qualify.perform();
  }

  get allocation() {
    return this.args.allocation;
  }

  get stats() {
    if (!this.allocation?.isRunning) return undefined;
    return this.statsTrackersRegistry.getTracker(this.allocation);
  }

  get showJobColumn() {
    return this.args.context === 'node';
  }

  // Makes sure the allocation is a complete record (preemption and
  // rescheduling are only in the full record) and that its job is loaded
  // before the stats are polled.
  @task({ restartable: true })
  *qualify() {
    const allocation = this.allocation;
    if (!allocation) return;
    try {
      if (allocation.isPartial) {
        yield this.store.findRecord('allocation', allocation.id, {
          backgroundReload: false,
        });
      }
      if (allocation.get('job.isPending')) {
        yield allocation.get('job');
      } else if (!allocation.get('taskGroup')) {
        const job = allocation.get('job.content');
        if (job?.isPartial) yield job.reload();
      }
    } catch (e) {
      // The row still renders what it has.
    }
    yield this.fetchStats.perform();
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
