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
 * A row of Storage::PluginAllocations. The allocation of a plugin
 * allocation is fetched imperatively (plugin allocations are fragments
 * without relationships); its resource usage is polled while it runs.
 */
export default class StoragePluginAllocationRow extends Component {
  @service store;
  @service('stats-trackers-registry') statsTrackersRegistry;

  @tracked allocation = null;
  @tracked statsError = false;

  enablePolling = !isTesting();

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  get stats() {
    if (!this.allocation?.isRunning) return undefined;
    return this.statsTrackersRegistry.getTracker(this.allocation);
  }

  @task({ restartable: true })
  *load() {
    const pluginAllocation = this.args.pluginAllocation;
    if (!pluginAllocation) return;
    try {
      let allocation = yield pluginAllocation.getAllocation();
      // The stats need the complete allocation and its job (the
      // reservations of the task group).
      if (allocation?.isPartial) {
        allocation = yield this.store.findRecord('allocation', allocation.id, {
          backgroundReload: false,
        });
      }
      if (allocation?.get('job.isPending')) yield allocation.get('job');
      this.allocation = allocation;
    } catch (e) {
      // The row keeps the plugin allocation's own fields.
      return;
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
    } while (this.enablePolling && !this.isDestroying);
  }
}
