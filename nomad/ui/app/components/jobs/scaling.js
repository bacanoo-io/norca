/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action, get } from '@ember/object';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';

const LIVE_LIMIT = 20;

function nanosToMs(value) {
  return value ? Math.floor(Number(value) / 1e6) : 0;
}

function formatDuration(nanos) {
  if (!nanos) return null;
  const seconds = Number(nanos) / 1e9;
  if (seconds < 60) return `${seconds}s`;
  if (seconds % 60 === 0 && seconds < 3600) return `${seconds / 60}m`;
  return `${Math.round(seconds / 60)}m`;
}

/**
 * The Scaling tab of the job page (UI plan, job details): per task group
 * the scaling policy (the typed policy of the built-in autoscaler or the
 * policy block of an external one) with its editor, the manual count, the
 * count over time from the scaling events and the decisions, live from the
 * Scaling topic of the event stream.
 *
 * The metric values the checks read are not recorded by the servers, so
 * the chart shows the count and the decisions, not the metric against its
 * target.
 */
export default class JobsScaling extends Component {
  @service token;
  @service eventStream;

  @tracked policies = {};
  @tracked policiesError = null;
  @tracked liveDecisions = [];
  @tracked editing = null;

  get job() {
    return this.args.job;
  }

  get namespace() {
    return this.job.get('namespace.name') || 'default';
  }

  get groups() {
    const scales = this.job.get('scaleState.taskGroupScales') || [];
    return (this.job.taskGroups || []).map((taskGroup) => {
      const scale = scales.find?.((s) => s.name === taskGroup.name);
      const events = (scale ? get(scale, 'events').toArray() : [])
        .slice()
        .sort((a, b) => b.time - a.time);
      const policy = this.policies[taskGroup.name] || null;
      return {
        taskGroup,
        name: taskGroup.name,
        scaling: taskGroup.scaling,
        policy,
        checks: (policy?.Checks || []).map((c) => ({
          name: c.Name,
          source: c.Source || 'nomad-apm',
          query: c.Query,
          strategy: c.Strategy?.Name,
          target: c.Strategy?.Config
            ? Object.entries(c.Strategy.Config)
                .map(([k, v]) => `${k} ${v}`)
                .join(', ')
            : '',
        })),
        interval: formatDuration(policy?.EvaluationInterval),
        cooldown: formatDuration(policy?.Cooldown),
        events,
        decisions: this.liveDecisions.filter((d) => d.Group === taskGroup.name),
        chart: this.chartFor(taskGroup, events),
      };
    });
  }

  get hasScaling() {
    return this.groups.some((g) => g.scaling);
  }

  // The count of the group over time: one step per scaling event with a
  // count, ending at the current count.
  chartFor(taskGroup, events) {
    const points = events
      .filter((e) => e.hasCount)
      .slice()
      .reverse()
      .map((e) => [e.time.getTime(), e.count]);
    if (!points.length) return null;
    points.push([Date.now(), taskGroup.count]);
    const errors = events
      .filter((e) => e.error)
      .map((e) => ({ xAxis: e.time.getTime() }));
    return {
      grid: { left: 40, right: 16, top: 16, bottom: 32 },
      tooltip: { trigger: 'axis' },
      xAxis: { type: 'time' },
      yAxis: {
        type: 'value',
        minInterval: 1,
        min: taskGroup.scaling?.min ?? 0,
        max: taskGroup.scaling?.max ?? undefined,
      },
      series: [
        {
          name: 'Count',
          type: 'line',
          step: 'end',
          showSymbol: true,
          data: points,
          markLine: errors.length
            ? { symbol: 'none', label: { show: false }, data: errors }
            : undefined,
        },
      ],
    };
  }

  chartSummary(group) {
    const counts = group.events.filter((e) => e.hasCount);
    if (!counts.length) return '';
    const first = counts[counts.length - 1];
    return `The count of ${group.name} changed ${counts.length} times, from ${first.previousCount} to ${group.taskGroup.count}.`;
  }

  async loadPolicies() {
    try {
      const response = await this.token.authorizedRequest(
        `/v1/scaling/policies?job=${encodeURIComponent(
          this.job.plainId
        )}&namespace=${encodeURIComponent(this.namespace)}`
      );
      if (!response.ok) {
        this.policiesError =
          response.status === 403
            ? 'Your token cannot read the scaling policies.'
            : `The scaling policies could not be loaded (${response.status}).`;
        return;
      }
      const stubs = (await response.json()) || [];
      const details = await Promise.all(
        stubs
          .filter((s) => (s.Type || 'horizontal') === 'horizontal')
          .map((s) =>
            this.token
              .authorizedRequest(
                `/v1/scaling/policy/${encodeURIComponent(
                  s.ID
                )}?namespace=${encodeURIComponent(this.namespace)}`
              )
              .then((r) => (r.ok ? r.json() : null))
          )
      );
      const policies = {};
      details.filter(Boolean).forEach((p) => {
        const group = p.Target?.Group;
        if (group) policies[group] = p;
      });
      if (!this.isDestroying && !this.isDestroyed) this.policies = policies;
    } catch (e) {
      this.policiesError = 'The scaling policies could not be loaded.';
    }
  }

  // The decisions of the built-in autoscaler (Scaling topic): live only,
  // the events of the job scale status remain the history.
  live = modifier(() => {
    waitForPromise(Promise.resolve().then(() => this.loadPolicies()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`Scaling:${this.job.plainId}`],
      region: this.job.region || '',
      namespace: this.namespace,
      onEvents: (events) => {
        const decisions = events
          .filter((e) => e.Topic === 'Scaling' && e.Payload)
          .map((e) => ({
            ...(e.Payload.ScalingDecision || e.Payload),
            Type: e.Type,
            Index: e.Index,
          }));
        if (decisions.length) {
          this.liveDecisions = [
            ...decisions.reverse(),
            ...this.liveDecisions,
          ].slice(0, LIVE_LIMIT);
        }
      },
    });
    return () => unsubscribe();
  });

  decisionTime(decision) {
    return nanosToMs(decision.Time);
  }

  @action
  editPolicy(group) {
    this.editing = group;
  }

  @action
  closeEditor() {
    this.editing = null;
  }

  @action
  async policySaved() {
    this.editing = null;
    await this.job.reload();
    await this.loadPolicies();
  }
}
