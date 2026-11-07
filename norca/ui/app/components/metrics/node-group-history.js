/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import { fetchMetrics } from 'nomad-ui/utils/metrics-history';

const CONDITION = 'norca.nodegroup.condition';

/**
 * The scale of a node group over time, from the metrics store of the
 * servers: the nodes it wants, the ready, joining and failed ones, the
 * rolling replacement, and when each condition was true. The condition types
 * are those the series list has for the group. Needs read on the NodeGroup
 * objects; without it the card is left out.
 *
 *   <Metrics::NodeGroupHistory @name="workers" />
 */
export default class MetricsNodeGroupHistory extends Component {
  @service token;

  @tracked conditionTypes = null;

  constructor() {
    super(...arguments);
    this.loadConditions.perform();
  }

  // The condition types of the group. Without the list (forbidden, or the
  // store disabled) the history card reports that itself.
  @task
  *loadConditions() {
    try {
      const series = yield fetchMetrics(this.token, 'series');
      this.conditionTypes = (series.Gauges || [])
        .filter(
          (g) => g.Metric === CONDITION && g.Labels?.group === this.args.name
        )
        .map((g) => g.Labels.type)
        .sort();
    } catch (e) {
      this.conditionTypes = [];
    }
  }

  get charts() {
    const labels = { group: this.args.name };
    const charts = [
      {
        id: 'nodes',
        title: 'Nodes',
        axes: [{ unit: 'count' }],
        series: [
          {
            name: 'Wanted',
            metric: 'norca.nodegroup.replicas',
            labels,
            step: true,
            dashed: true,
          },
          {
            name: 'Ready',
            metric: 'norca.nodegroup.ready',
            labels,
            step: true,
          },
          {
            name: 'Joining',
            metric: 'norca.nodegroup.joining',
            labels,
            step: true,
          },
          {
            name: 'Failed',
            metric: 'norca.nodegroup.failed',
            labels,
            step: true,
          },
        ],
      },
      {
        id: 'rollout',
        title: 'Rolling replacement',
        axes: [{ unit: 'count' }],
        series: [
          {
            name: 'Replacing',
            metric: 'norca.nodegroup.rollout_replacing',
            labels,
            step: true,
          },
          {
            name: 'Outdated',
            metric: 'norca.nodegroup.rollout_outdated',
            labels,
            step: true,
            dashed: true,
          },
        ],
      },
    ];
    if (this.conditionTypes?.length) {
      charts.push({
        id: 'conditions',
        title: 'Conditions (when true)',
        band: true,
        series: this.conditionTypes.map((type) => ({
          name: type,
          metric: CONDITION,
          labels: { group: this.args.name, type },
        })),
      });
    }
    return charts;
  }
}
