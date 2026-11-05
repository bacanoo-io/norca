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

const METRICS = {
  cpuAllocated: 'norca.cluster.cpu_allocated',
  cpuUsed: 'norca.cluster.cpu_used',
  memoryAllocated: 'norca.cluster.memory_allocated',
  memoryUsed: 'norca.cluster.memory_used',
};

/**
 * The capacity of a datacenter and node pool over time, from the metrics
 * store of the servers: the CPU and memory allocated to the running
 * allocations against what they actually use (overcommit), in percent of
 * the capacity of the ready nodes. Shown only to tokens that may read the
 * nodes.
 *
 *   <Metrics::ClusterHistory />
 */
export default class MetricsClusterHistory extends Component {
  @service token;

  @tracked range = '24h';
  @tracked scopes = [];
  @tracked scope = null;
  @tracked history = {};
  @tracked state = 'loading'; // loading, ready, none, disabled, error
  @tracked error = null;

  constructor() {
    super(...arguments);
    this.loadScopes.perform();
  }

  // Without node read access, or without capacity series, the card is left
  // out.
  get isShown() {
    return this.state !== 'none';
  }

  get tabs() {
    return RANGES.map((r) => ({ id: r.id, label: r.label }));
  }

  get scopeOptions() {
    return this.scopes.map((s) => ({ value: s.key, label: s.label }));
  }

  @action
  selectRange(id) {
    this.range = id;
    this.load.perform();
  }

  @action
  selectScope(key) {
    this.scope = this.scopes.find((s) => s.key === key) || null;
    this.load.perform();
  }

  @task({ restartable: true })
  *loadScopes() {
    try {
      const series = yield fetchMetrics(this.token, 'series');
      if (!series.Enabled) {
        this.state = 'disabled';
        return;
      }
      const scopes = (series.Gauges || [])
        .filter((g) => g.Metric === METRICS.cpuUsed && !g.Labels.class)
        .map((g) => ({
          key: `${g.Labels.dc}/${g.Labels.pool}`,
          dc: g.Labels.dc,
          pool: g.Labels.pool,
          label: `${g.Labels.dc} / ${g.Labels.pool}`,
        }))
        .sort((a, b) => a.label.localeCompare(b.label));
      if (!scopes.length) {
        this.state = 'none';
        return;
      }
      this.scopes = scopes;
      this.scope = scopes[0];
      yield this.load.perform();
    } catch (e) {
      // Without node read access the card is left out.
      this.state = e instanceof MetricsForbidden ? 'none' : 'error';
      this.error = e.message;
    }
  }

  @task({ restartable: true })
  *load() {
    if (!this.scope) return;
    const base = {
      dc: this.scope.dc,
      pool: this.scope.pool,
      from: rangeById(this.range).from,
      max_points: 240,
    };
    try {
      const names = Object.keys(METRICS);
      const results = yield Promise.all(
        names.map((name) =>
          fetchMetrics(this.token, 'history', {
            ...base,
            metric: METRICS[name],
          })
        )
      );
      const history = {};
      names.forEach((name, i) => (history[name] = results[i]));
      this.history = history;
      this.state = 'ready';
    } catch (e) {
      this.state = e instanceof MetricsForbidden ? 'none' : 'error';
      this.error = e.message;
    }
  }

  line(name, data, dashed) {
    return {
      name,
      type: 'line',
      showSymbol: false,
      lineStyle: { width: 1.5, type: dashed ? 'dashed' : 'solid' },
      data,
    };
  }

  get option() {
    const h = this.history;
    return {
      animation: false,
      legend: { bottom: 0, icon: 'roundRect' },
      tooltip: {
        trigger: 'axis',
        valueFormatter: (value) => (value == null ? '–' : `${value}%`),
      },
      grid: { left: 8, right: 8, top: 16, bottom: 32, containLabel: true },
      xAxis: { type: 'time', axisLabel: { hideOverlap: true } },
      yAxis: { type: 'value', min: 0, axisLabel: { formatter: '{value}%' } },
      series: [
        this.line('CPU allocated', points(h.cpuAllocated), true),
        this.line('CPU used', points(h.cpuUsed)),
        this.line('Memory allocated', points(h.memoryAllocated), true),
        this.line('Memory used', points(h.memoryUsed)),
      ],
    };
  }

  get summaryText() {
    const last = (h) => {
      const list = points(h);
      return list.length ? list[list.length - 1][1] : 0;
    };
    const h = this.history;
    return `CPU ${last(h.cpuAllocated)}% allocated, ${last(
      h.cpuUsed
    )}% used; memory ${last(h.memoryAllocated)}% allocated, ${last(
      h.memoryUsed
    )}% used.`;
  }
}
