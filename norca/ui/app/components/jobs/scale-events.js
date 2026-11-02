/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

/**
 *   <Jobs::ScaleEvents @events={{this.sortedScaleEvents}} />
 *
 * @events: the scale events of a task group, newest first.
 */
export default class JobsScaleEvents extends Component {
  @tracked open = [];

  get events() {
    return this.args.events || [];
  }

  // The timeline is useful when most events changed the count.
  get showTimeline() {
    const counts = this.events.filter((e) => e.hasCount).length;
    return counts > 1 && counts >= this.events.length / 2;
  }

  get chart() {
    const points = this.events
      .filter((e) => e.hasCount)
      .slice()
      .reverse()
      .map((e) => [e.time.getTime(), e.count]);
    const marks = this.events
      .filter((e) => !e.hasCount)
      .map((e) => ({ xAxis: e.time.getTime(), name: e.message }));
    return {
      grid: { left: 40, right: 16, top: 16, bottom: 32 },
      tooltip: { trigger: 'axis' },
      xAxis: { type: 'time' },
      yAxis: { type: 'value', minInterval: 1 },
      series: [
        {
          name: 'Count',
          type: 'line',
          step: 'end',
          areaStyle: { opacity: 0.15 },
          data: points,
          markLine: marks.length
            ? { symbol: 'none', label: { show: false }, data: marks }
            : undefined,
        },
      ],
    };
  }

  // The events without a count, marked on the timeline.
  get annotations() {
    return this.events.filter((e) => !e.hasCount).map((e) => e.message);
  }

  get summary() {
    const counts = this.events.filter((e) => e.hasCount);
    return `${counts.length} count changes; the latest count is ${counts[0]?.count}.`;
  }

  metaText = (event) => JSON.stringify(event.meta, null, 2);

  @action
  toggle(uid) {
    this.open = this.open.includes(uid)
      ? this.open.filter((u) => u !== uid)
      : [...this.open, uid];
  }
}
