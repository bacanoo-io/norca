/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { get } from '@ember/object';
import { formatBytes, formatHertz } from 'nomad-ui/utils/units';

/**
 *   <Allocations::Usage @metric="cpu" @isRunning={{true}} @tracker={{stats}} />
 *
 * @tracker is an allocation stats tracker, or the stats of one task
 * (`tracker.tasks.findBy('task', name)`) with @reserved* of the task.
 */
export default class AllocationsUsage extends Component {
  // Read through get(): the series are Ember arrays the tracker pushes to.
  get stat() {
    const tracker = this.args.tracker;
    if (!tracker) return null;
    const metric = this.args.metric === 'cpu' ? 'cpu' : 'memory';
    return get(tracker, `${metric}.lastObject`) || null;
  }

  get percent() {
    return Math.round((this.stat?.percent || 0) * 1000) / 10;
  }

  get reserved() {
    const tracker = this.args.tracker;
    if (this.args.metric === 'cpu') {
      return this.args.reservedCPU ?? (tracker && get(tracker, 'reservedCPU'));
    }
    return (
      this.args.reservedMemory ?? (tracker && get(tracker, 'reservedMemory'))
    );
  }

  get text() {
    if (!this.stat) return '';
    if (this.args.metric === 'cpu') {
      return `${formatHertz(this.stat.used, 'MHz')} / ${formatHertz(
        this.reserved,
        'MHz'
      )}`;
    }
    return `${formatBytes(this.stat.used)} / ${formatBytes(
      this.reserved,
      'MiB'
    )}`;
  }
}
