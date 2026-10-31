/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import {
  RANGES,
  rangeById,
  fetchMetrics,
  points,
  MetricsForbidden,
} from 'nomad-ui/utils/metrics-history';

/**
 * The CPU and memory use of the target group of a scaling policy, in percent
 * of the allocation, against its running allocations: whether the scaling
 * relieved the load.
 *
 *   <Metrics::ScalingHistory @namespace="default" @job="api" @group="web" />
 */
export default class MetricsScalingHistory extends Component {
  @service token;

  @tracked range = '24h';
  @tracked history = {};
  @tracked state = 'loading'; // loading, ready, disabled, forbidden, error
  @tracked error = null;

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  get tabs() {
    return RANGES.map((r) => ({ id: r.id, label: r.label }));
  }

  @action
  selectRange(id) {
    this.range = id;
    this.load.perform();
  }

  @task({ restartable: true })
  *load() {
    const base = {
      namespace: this.args.namespace,
      job: this.args.job,
      group: this.args.group,
      from: rangeById(this.range).from,
      max_points: 240,
    };
    try {
      const [cpu, memory, running] = yield Promise.all(
        [
          'norca.group.cpu',
          'norca.group.memory',
          'norca.group.allocs_running',
        ].map((metric) =>
          fetchMetrics(this.token, 'history', { ...base, metric })
        )
      );
      if (!cpu.Enabled) {
        this.state = 'disabled';
        return;
      }
      this.history = { cpu, memory, running };
      this.state = 'ready';
    } catch (e) {
      this.state = e instanceof MetricsForbidden ? 'forbidden' : 'error';
      this.error = e.message;
    }
  }

  get isEmpty() {
    const h = this.history;
    return !(h.cpu?.Points?.length || h.running?.Points?.length);
  }

  get option() {
    const h = this.history;
    return {
      animation: false,
      legend: { bottom: 0, icon: 'roundRect' },
      tooltip: { trigger: 'axis' },
      grid: { left: 8, right: 8, top: 16, bottom: 32, containLabel: true },
      xAxis: { type: 'time', axisLabel: { hideOverlap: true } },
      yAxis: [
        { type: 'value', min: 0, axisLabel: { formatter: '{value}%' } },
        {
          type: 'value',
          min: 0,
          minInterval: 1,
          splitLine: { show: false },
        },
      ],
      series: [
        {
          name: 'CPU',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 1.5 },
          data: points(h.cpu),
        },
        {
          name: 'Memory',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 1.5 },
          data: points(h.memory),
        },
        {
          name: 'Running allocations',
          type: 'line',
          step: 'end',
          yAxisIndex: 1,
          showSymbol: false,
          lineStyle: { width: 2 },
          data: points(h.running),
        },
      ],
    };
  }

  get summaryText() {
    const last = (list) => (list.length ? list[list.length - 1][1] : 0);
    return `Latest: CPU ${last(points(this.history.cpu))}%, memory ${last(
      points(this.history.memory)
    )}% of the allocation, ${last(
      points(this.history.running)
    )} running allocations.`;
  }
}
