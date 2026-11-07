/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { tracked } from '@glimmer/tracking';
import { task } from 'ember-concurrency';
import MetricsComponent from 'nomad-ui/utils/metrics-component';
import { points, MetricsForbidden } from 'nomad-ui/utils/metrics-history';
import { formatMetric } from 'nomad-ui/utils/metrics-format';

// sumHistories adds up histories of the same range point by point (the
// series of several namespaces, for example).
export function sumHistories(histories) {
  const first = histories.find((h) => h && h.Enabled) || histories[0];
  const byTime = new Map();
  histories.forEach((h) =>
    ((h && h.Points) || []).forEach((p) => {
      const sum = byTime.get(p.Time) || { Time: p.Time, Avg: 0, Max: 0 };
      sum.Avg += p.Avg;
      sum.Max += p.Max;
      byTime.set(p.Time, sum);
    })
  );
  const Points = [...byTime.values()].sort(
    (a, b) => Date.parse(a.Time) - Date.parse(b.Time)
  );
  return { ...first, Points };
}

// bandIntervals returns the [category, start, end] intervals (ms) in which
// a 0/1 history was 1 at some time of a step, consecutive steps merged.
export function bandIntervals(history, category) {
  const stepMs = (history?.Step || 0) / 1e6 || 30e3;
  const out = [];
  ((history && history.Points) || []).forEach((p) => {
    if (!(p.Max > 0)) return;
    const start = Date.parse(p.Time);
    const last = out[out.length - 1];
    if (last && last[2] >= start) {
      last[2] = start + stepMs;
    } else {
      out.push([category, start, start + stepMs]);
    }
  });
  return out;
}

function renderBand(params, api) {
  const category = api.value(0);
  const start = api.coord([api.value(1), category]);
  const end = api.coord([api.value(2), category]);
  const height = api.size([0, 1])[1] * 0.6;
  return {
    type: 'rect',
    shape: {
      x: start[0],
      y: start[1] - height / 2,
      width: Math.max(end[0] - start[0], 2),
      height,
    },
    style: api.style(),
  };
}

/**
 * A card of history charts of gauge series of the metrics store, with the
 * range selector. Every chart reads its series with the history API; a chart
 * the token may not read (403) is left out, and the card when it may read
 * none. A chart without points says so instead of drawing an empty chart.
 *
 *   <Metrics::GaugeHistory @title="Host" @charts={{this.charts}}
 *     @serverId="server-1.global" @region="global" @range="24h" />
 *
 * A chart: { id, title, axes: [{ unit, min, max }], band, series }. A series:
 * { name, metric, labels, prop ('Avg' or 'Max'), step, dashed, area, axis,
 * optional }. labels may be a list of label sets, whose histories are added
 * up. An optional series without points is left out. A band chart draws
 * 0/1 series (conditions) as the time ranges in which they were 1.
 * Units: see formatMetric (utils/metrics-format.js).
 */
export default class MetricsGaugeHistory extends MetricsComponent {
  @tracked results = [];
  @tracked state = 'loading'; // loading, ready, disabled, forbidden, error
  @tracked error = null;
  @tracked loadedAt = Date.now();

  constructor() {
    super(...arguments);
    if (this.args.range) this.range = this.args.range;
    this.load.perform();
  }

  get charts() {
    return this.args.charts || [];
  }

  get isShown() {
    return this.state !== 'forbidden';
  }

  get columnClass() {
    return this.args.columnClass || 'col-lg-6';
  }

  get chartHeight() {
    return this.args.chartHeight || 200;
  }

  async loadSeries(series, from) {
    const lists = Array.isArray(series.labels)
      ? series.labels
      : [series.labels || {}];
    const histories = await Promise.all(
      lists.map((labels) =>
        this.fetch('history', {
          ...labels,
          metric: series.metric,
          from,
          max_points: 240,
        })
      )
    );
    return histories.length === 1 ? histories[0] : sumHistories(histories);
  }

  async loadChart(chart, from) {
    try {
      const histories = await Promise.all(
        chart.series.map((s) => this.loadSeries(s, from))
      );
      return { state: 'ready', histories };
    } catch (e) {
      return {
        state: e instanceof MetricsForbidden ? 'forbidden' : 'error',
        error: e.message,
      };
    }
  }

  @task({ restartable: true })
  *load() {
    const from = this.selectedRange.from;
    const results = yield Promise.all(
      this.charts.map((chart) => this.loadChart(chart, from))
    );
    this.loadedAt = Date.now();
    const ready = results.filter((r) => r.state === 'ready');
    if (
      ready.length &&
      ready.every((r) => !r.histories.some((h) => h?.Enabled))
    ) {
      this.state = 'disabled';
      return;
    }
    this.results = results;
    if (ready.length) {
      this.state = 'ready';
    } else if (results.some((r) => r.state === 'error')) {
      this.state = 'error';
      this.error = results.find((r) => r.state === 'error').error;
    } else {
      this.state = 'forbidden';
    }
  }

  get views() {
    return this.charts
      .map((chart, i) => ({ chart, result: this.results[i] }))
      .filter(({ result }) => result && result.state !== 'forbidden')
      .map(({ chart, result }) => this.view(chart, result));
  }

  get isEmpty() {
    const views = this.views;
    return views.length > 0 && views.every((v) => v.empty);
  }

  view(chart, result) {
    if (result.state === 'error') {
      return { id: chart.id, title: chart.title, error: result.error };
    }
    const series = chart.series
      .map((spec, i) => ({ spec, history: result.histories[i] }))
      .filter(({ spec, history }) => !spec.optional || history?.Points?.length);
    const empty = !series.some(({ history }) => history?.Points?.length);
    return {
      id: chart.id,
      title: chart.title,
      empty,
      option: empty
        ? null
        : chart.band
        ? this.bandOption(series)
        : this.lineOption(chart, series),
      summary: empty ? '' : this.summary(chart, series),
    };
  }

  lineOption(chart, series) {
    const axes = chart.axes || [{ unit: 'count' }];
    const unitOf = (spec) => (axes[spec.axis || 0] || axes[0]).unit;
    return {
      animation: false,
      legend: { bottom: 0, icon: 'roundRect' },
      tooltip: {
        trigger: 'axis',
        formatter: (params) => {
          const list = Array.isArray(params) ? params : [params];
          if (!list.length) return '';
          const time = new Date(list[0].value[0]).toLocaleString();
          return [time]
            .concat(
              list.map(
                (p) =>
                  `${p.marker}${p.seriesName}: ${formatMetric(
                    p.value[1],
                    unitOf(series[p.seriesIndex].spec)
                  )}`
              )
            )
            .join('<br>');
        },
      },
      grid: { left: 8, right: 8, top: 16, bottom: 32, containLabel: true },
      xAxis: { type: 'time', axisLabel: { hideOverlap: true } },
      yAxis: axes.map((axis, i) => ({
        type: 'value',
        min: axis.min ?? 0,
        max: axis.max,
        minInterval:
          axis.unit === 'count' || axis.unit === 'state' ? 1 : undefined,
        splitLine: i ? { show: false } : undefined,
        axisLabel: { formatter: (v) => formatMetric(v, axis.unit) },
      })),
      series: series.map(({ spec, history }) => ({
        name: spec.name,
        type: 'line',
        step: spec.step ? 'end' : undefined,
        yAxisIndex: spec.axis || 0,
        showSymbol: false,
        lineStyle: {
          width: spec.area ? 0 : 1.5,
          type: spec.dashed ? 'dashed' : 'solid',
        },
        areaStyle: spec.area ? { opacity: 0.2 } : undefined,
        data: points(history, spec.prop),
      })),
    };
  }

  bandOption(series) {
    const categories = series.map(({ spec }) => spec.name);
    const data = series.flatMap(({ history }, i) => bandIntervals(history, i));
    const to = this.loadedAt;
    const from = to - this.selectedRange.seconds * 1000;
    const time = (ms) => new Date(ms).toLocaleString();
    return {
      animation: false,
      tooltip: {
        trigger: 'item',
        formatter: (p) =>
          `${categories[p.value[0]]}: ${time(p.value[1])} – ${time(
            p.value[2]
          )}`,
      },
      grid: { left: 8, right: 16, top: 8, bottom: 8, containLabel: true },
      xAxis: {
        type: 'time',
        min: from,
        max: to,
        axisLabel: { hideOverlap: true },
      },
      yAxis: { type: 'category', data: categories, inverse: true },
      series: [
        {
          type: 'custom',
          renderItem: renderBand,
          encode: { x: [1, 2], y: 0 },
          itemStyle: { color: '#f76707', opacity: 0.8 },
          data,
        },
      ],
    };
  }

  summary(chart, series) {
    if (chart.band) {
      const on = series
        .filter(({ history }) => bandIntervals(history, 0).length)
        .map(({ spec }) => spec.name);
      return on.length
        ? `${chart.title}: true at times in the range: ${on.join(', ')}.`
        : `${chart.title}: none was true in the range.`;
    }
    const axes = chart.axes || [{ unit: 'count' }];
    const latest = series.map(({ spec, history }) => {
      const list = points(history, spec.prop);
      const unit = (axes[spec.axis || 0] || axes[0]).unit;
      return `${spec.name} ${
        list.length ? formatMetric(list[list.length - 1][1], unit) : '–'
      }`;
    });
    return `${chart.title}, latest: ${latest.join(', ')}.`;
  }
}
