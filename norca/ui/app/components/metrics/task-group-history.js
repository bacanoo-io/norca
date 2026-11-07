/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { tracked } from '@glimmer/tracking';
import { task } from 'ember-concurrency';
import MetricsComponent from 'nomad-ui/utils/metrics-component';
import {
  points,
  recommend,
  MetricsForbidden,
} from 'nomad-ui/utils/metrics-history';
import {
  formatScheduledBytes,
  formatScheduledHertz,
} from 'nomad-ui/utils/units';

const METRICS = {
  cpu: 'norca.group.cpu_mhz',
  throttled: 'norca.group.cpu_throttled',
  memory: 'norca.group.memory_mb',
  cache: 'norca.group.memory_cache_mb',
  oom: 'norca.group.oom_kills',
};

/**
 * The usage history of a task group from the metrics store of the servers:
 * CPU (average, peak, throttling), memory (average, peak, page cache, OOM
 * kills) per allocation, and the right-sizing view (the 95th percentile of
 * the CPU peaks and the memory peak of the range against the allocation).
 *
 *   <Metrics::TaskGroupHistory @namespace="default" @job="api" @group="web"
 *     @reservedCPU={{500}} @reservedMemory={{256}} />
 */
export default class MetricsTaskGroupHistory extends MetricsComponent {
  @tracked range = '1h';
  @tracked history = {};
  @tracked summary = null;
  @tracked state = 'loading'; // loading, ready, disabled, forbidden, error
  @tracked error = null;

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  @task({ restartable: true })
  *load() {
    const range = this.selectedRange;
    const base = {
      namespace: this.args.namespace,
      job: this.args.job,
      group: this.args.group,
      from: range.from,
      max_points: 240,
    };
    try {
      const names = Object.keys(METRICS);
      const results = yield Promise.all(
        names.map((name) =>
          this.fetch('history', {
            ...base,
            metric: METRICS[name],
          })
        )
      );
      if (!results[0].Enabled) {
        this.state = 'disabled';
        return;
      }
      const history = {};
      names.forEach((name, i) => (history[name] = results[i]));
      this.history = history;
      this.summary = yield this.fetch('summary', {
        namespace: this.args.namespace,
        job: this.args.job,
        group: this.args.group,
        window: `${range.seconds}s`,
      });
      this.state = 'ready';
    } catch (e) {
      this.state = e instanceof MetricsForbidden ? 'forbidden' : 'error';
      this.error = e.message;
    }
  }

  get isEmpty() {
    return !(this.history.cpu?.Points || []).length;
  }

  get oomTimes() {
    return (this.history.oom?.Points || [])
      .filter((p) => p.Avg > 0)
      .map((p) => ({ time: Date.parse(p.Time), kills: p.Avg }));
  }

  get oomKills() {
    return this.oomTimes.reduce((sum, p) => sum + p.kills, 0);
  }

  axisOption(series, yAxis) {
    return {
      animation: false,
      legend: { bottom: 0, icon: 'roundRect' },
      tooltip: { trigger: 'axis' },
      grid: { left: 8, right: 8, top: 16, bottom: 32, containLabel: true },
      xAxis: { type: 'time', axisLabel: { hideOverlap: true } },
      yAxis,
      series,
    };
  }

  reservedLine(value, label) {
    if (!(value > 0)) return undefined;
    return {
      symbol: 'none',
      silent: true,
      label: { formatter: label, position: 'insideEndTop' },
      lineStyle: { type: 'dashed' },
      data: [{ yAxis: value }],
    };
  }

  get cpuOption() {
    const h = this.history;
    return this.axisOption(
      [
        {
          name: 'Average',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 1.5 },
          data: points(h.cpu),
          markLine: this.reservedLine(this.args.reservedCPU, 'Reserved'),
        },
        {
          name: 'Peak',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 0 },
          areaStyle: { opacity: 0.2 },
          data: points(h.cpu, 'Max'),
        },
        {
          name: 'Throttled',
          type: 'line',
          yAxisIndex: 1,
          showSymbol: false,
          lineStyle: { width: 1, type: 'dotted' },
          data: points(h.throttled),
        },
      ],
      [
        { type: 'value', axisLabel: { formatter: '{value} MHz' } },
        {
          type: 'value',
          min: 0,
          max: 100,
          splitLine: { show: false },
          axisLabel: { formatter: '{value}%' },
        },
      ]
    );
  }

  get memoryOption() {
    const h = this.history;
    const oom = this.oomTimes;
    return this.axisOption(
      [
        {
          name: 'Average',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 1.5 },
          data: points(h.memory),
          markLine: this.reservedLine(this.args.reservedMemory, 'Reserved'),
        },
        {
          name: 'Peak',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 0 },
          areaStyle: { opacity: 0.2 },
          data: points(h.memory, 'Max'),
        },
        {
          name: 'Page cache',
          type: 'line',
          showSymbol: false,
          lineStyle: { width: 1, type: 'dotted' },
          data: points(h.cache),
          markLine: oom.length
            ? {
                symbol: 'none',
                silent: true,
                label: { formatter: 'OOM', position: 'end' },
                lineStyle: { type: 'solid', color: '#d63939' },
                data: oom.map((p) => ({ xAxis: p.time })),
              }
            : undefined,
        },
      ],
      { type: 'value', axisLabel: { formatter: '{value} MiB' } }
    );
  }

  get rightSizing() {
    const s = this.summary;
    if (!s || !s.Enabled) return null;
    const cpu = s.Metrics?.[METRICS.cpu];
    const memory = s.Metrics?.[METRICS.memory];
    if (!cpu?.Samples || !memory?.Samples) return null;
    const rows = [];
    const cpuSuggested = recommend(cpu.P95, 50);
    rows.push({
      key: 'cpu',
      label: 'CPU',
      allocated: formatScheduledHertz(s.AllocatedCPUMHz, 'MHz'),
      measured: `${formatScheduledHertz(
        cpu.P95,
        'MHz'
      )} (95th percentile of the peaks)`,
      suggested: formatScheduledHertz(cpuSuggested, 'MHz'),
      change: this.change(s.AllocatedCPUMHz, cpuSuggested),
    });
    const memorySuggested = recommend(memory.Max, 16);
    rows.push({
      key: 'memory',
      label: 'Memory',
      allocated: formatScheduledBytes(s.AllocatedMemoryMB, 'MiB'),
      measured: `${formatScheduledBytes(memory.Max, 'MiB')} (peak)`,
      suggested: formatScheduledBytes(memorySuggested, 'MiB'),
      change: this.change(s.AllocatedMemoryMB, memorySuggested),
    });
    return rows;
  }

  change(allocated, suggested) {
    if (!(allocated > 0) || !(suggested > 0)) return '';
    const percent = Math.round(((suggested - allocated) / allocated) * 100);
    if (Math.abs(percent) < 10) return 'about right';
    return percent < 0 ? `${-percent}% less` : `${percent}% more`;
  }

  get summaryText() {
    const cpu = points(this.history.cpu);
    const memory = points(this.history.memory);
    const last = (list) => (list.length ? list[list.length - 1][1] : 0);
    return `Latest average per allocation: ${formatScheduledHertz(
      last(cpu),
      'MHz'
    )} CPU, ${formatScheduledBytes(last(memory), 'MiB')} memory. ${
      this.oomKills
    } OOM kills in the range.`;
  }
}
