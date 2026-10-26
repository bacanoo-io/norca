/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import {
  formatScheduledBytes,
  formatScheduledHertz,
} from 'nomad-ui/utils/units';

const COLORS = { cpu: 'blue', memory: 'purple' };

/**
 * The presentation of a primary metric (CPU or memory of an allocation, a
 * task or a client): the recent history as a chart, the current use as a
 * meter and the absolute figures.
 *
 *   <PrimaryMetric::View
 *     @metric="cpu"
 *     @data={{this.data}}            (frames: { timestamp, used, percent })
 *     @series={{this.series}}        (optional, stacked: [{ name, data }])
 *     @reserved={{this.reservedAmount}}
 *     @annotations={{this.reservedAnnotations}} ([{ label, percent }])
 *   />
 */
export default class PrimaryMetricView extends Component {
  get title() {
    if (this.args.metric === 'cpu') return 'CPU';
    if (this.args.metric === 'memory') return 'Memory';
    return this.args.metric;
  }

  get color() {
    return COLORS[this.args.metric] || 'azure';
  }

  get current() {
    const data = this.args.data || [];
    return data[data.length - 1] || null;
  }

  get percent() {
    return this.current?.percent || 0;
  }

  get usedText() {
    const used = this.current?.used;
    if (this.args.metric === 'cpu') return formatScheduledHertz(used, 'MHz');
    if (this.args.metric === 'memory') return formatScheduledBytes(used);
    return `${used ?? ''}`;
  }

  get reservedText() {
    const reserved = this.args.reserved;
    if (this.args.metric === 'cpu')
      return formatScheduledHertz(reserved, 'MHz');
    if (this.args.metric === 'memory') {
      return formatScheduledBytes(reserved, 'MiB');
    }
    return `${reserved ?? ''}`;
  }

  get summary() {
    return `${this.title}: ${this.usedText} of ${
      this.reservedText
    } (${Math.round(this.percent * 100)}%)`;
  }

  toPoints(data, prop = 'percent') {
    return (data || []).map((frame) => [
      frame.timestamp,
      frame[prop] == null ? null : Math.round(frame[prop] * 10000) / 100,
    ]);
  }

  get option() {
    const stacked = (this.args.series || []).length > 0;
    const series = stacked
      ? this.args.series.map((s) => ({
          name: s.name,
          type: 'line',
          stack: 'total',
          showSymbol: false,
          areaStyle: { opacity: 0.35 },
          lineStyle: { width: 1.5 },
          data: this.toPoints(s.data, 'percentTotal'),
        }))
      : [
          {
            name: this.title,
            type: 'line',
            showSymbol: false,
            areaStyle: { opacity: 0.25 },
            lineStyle: { width: 1.5 },
            data: this.toPoints(this.args.data),
          },
        ];
    const annotations = this.args.annotations || [];
    if (annotations.length) {
      series[0].markLine = {
        symbol: 'none',
        silent: true,
        label: { formatter: '{b}', position: 'insideEndTop' },
        lineStyle: { type: 'dashed' },
        data: annotations.map((a) => ({
          name: a.label,
          yAxis: Math.round(a.percent * 10000) / 100,
        })),
      };
    }
    return {
      animation: false,
      tooltip: {
        trigger: 'axis',
        valueFormatter: (value) => (value == null ? '–' : `${value}%`),
      },
      grid: { left: 8, right: 8, top: 12, bottom: 8, containLabel: true },
      xAxis: { type: 'time', axisLabel: { hideOverlap: true } },
      yAxis: {
        type: 'value',
        min: 0,
        max: (extent) => Math.max(100, Math.ceil(extent.max)),
        axisLabel: { formatter: '{value}%' },
      },
      series,
    };
  }
}
