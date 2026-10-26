/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getPolicy,
  jobScale,
  listPolicies,
  eventItem,
  decisionItem,
  formatNs,
} from 'nomad-ui/utils/scaling';

/**
 * A scaling policy: its target, engine, bounds, timing and checks, the
 * count of the group over its recent scaling events and the decisions.
 */
export default class ScalingPolicy extends Component {
  @service token;
  @service system;
  @service eventStream;

  @tracked policy = null;
  @tracked group = null;
  @tracked engine = '';
  @tracked live = [];
  @tracked isLoading = true;
  @tracked loadError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.args.namespace || 'default';
  }

  get target() {
    return this.policy?.Target || {};
  }

  get jobRouteId() {
    return `${this.target.Job}@${this.target.Namespace || 'default'}`;
  }

  get details() {
    const p = this.policy;
    if (!p) return [];
    return [
      {
        title: 'Engine',
        value:
          this.engine === 'builtin'
            ? 'built-in (on the leader)'
            : 'external autoscaler',
      },
      { title: 'Enabled', value: p.Enabled ? 'yes' : 'no' },
      { title: 'Min / max', value: `${p.Min} / ${p.Max}` },
      {
        title: 'Current',
        value: this.group
          ? `${this.group.Running} running of ${this.group.Desired}`
          : '–',
      },
      { title: 'Evaluation interval', value: formatNs(p.EvaluationInterval) },
      { title: 'Cooldown', value: formatNs(p.Cooldown) },
      {
        title: 'Cooldown after scaling up',
        value: p.CooldownOnScaleUp ? formatNs(p.CooldownOnScaleUp) : 'same',
      },
      { title: 'On check error', value: p.OnCheckError || 'fail' },
      {
        title: 'During deployments',
        value:
          p.ScaleDuringDeployment === 'up_only'
            ? 'scale up only'
            : 'do not scale',
      },
    ];
  }

  get checks() {
    return (this.policy?.Checks || []).map((c) => ({
      ...c,
      strategy: [
        c.Strategy,
        Object.entries(c.StrategyConfig || {})
          .map(([k, v]) => `${k}=${v}`)
          .join(', '),
      ]
        .filter(Boolean)
        .join(': '),
      metric: c.Query || c.Metric || '–',
    }));
  }

  get events() {
    const ctx = {
      job: this.target.Job,
      group: this.target.Group,
      namespace: this.target.Namespace,
      region: this.region,
    };
    const persisted = (this.group?.Events || []).map((e) => eventItem(e, ctx));
    return [...this.live, ...persisted].sort(
      (a, b) => b.timestamp - a.timestamp
    );
  }

  get countSeries() {
    return this.events
      .filter((e) => e.count !== null && e.count !== undefined && e.timestamp)
      .slice()
      .reverse();
  }

  get chartOption() {
    const points = this.countSeries.map((e) => [e.timestamp, e.count]);
    const p = this.policy || {};
    return {
      tooltip: { trigger: 'axis' },
      grid: { left: 40, right: 20, top: 20, bottom: 30 },
      xAxis: { type: 'time' },
      // The axis reaches the maximum of the policy, so its line shows.
      yAxis: {
        type: 'value',
        minInterval: 1,
        min: 0,
        max: (extent) => Math.max(extent.max, p.Max || 0),
      },
      series: [
        {
          name: 'Count',
          type: 'line',
          step: 'end',
          data: points,
          markLine: {
            symbol: 'none',
            label: { formatter: '{b}' },
            lineStyle: { type: 'dashed' },
            data: [
              { name: 'min', yAxis: p.Min },
              { name: 'max', yAxis: p.Max },
            ],
          },
        },
      ],
    };
  }

  get chartSummary() {
    return this.countSeries
      .map((e) => `${e.time}: ${e.previous} to ${e.count}`)
      .join('; ');
  }

  get policyText() {
    return this.policy?.Policy || {};
  }

  async load() {
    try {
      const { body } = await getPolicy(
        this.token,
        this.region,
        this.args.policyId,
        this.namespace
      );
      this.policy = body;
      const t = body.Target || {};
      const [scale, builtin] = await Promise.all([
        jobScale(this.token, this.region, t.Job, t.Namespace || 'default')
          .then((r) => r.body)
          .catch(() => null),
        listPolicies(this.token, this.region, {
          namespace: t.Namespace || 'default',
          job: t.Job,
          engine: 'builtin',
        })
          .then((r) => r.body || [])
          .catch(() => []),
      ]);
      this.group = scale?.TaskGroups?.[t.Group] || null;
      this.engine = builtin.some((p) => p.ID === body.ID)
        ? 'builtin'
        : 'external';
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The scaling policy does not exist. It is removed with the scaling block of its job.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [policyId, region, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['Scaling'],
      region,
      namespace,
      onEvents: (events) => {
        const mine = events.filter(
          (e) =>
            (e.FilterKeys || []).includes(policyId) ||
            e.Payload?.PolicyID === policyId
        );
        if (mine.length) {
          this.live = [...mine.map(decisionItem), ...this.live].slice(0, 20);
          waitForPromise(this.load());
        }
      },
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });
}
