/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { htmlSafe } from '@ember/template';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import {
  fetchRegionSummary,
  unreachableRegion,
  buildWarnings,
  totals,
} from 'nomad-ui/utils/dashboard-data';
import { summarizeEvent } from 'nomad-ui/utils/event-summary';

const TOPICS = ['Job', 'Deployment', 'Evaluation', 'Allocation', 'Node'];
const REFRESH_DEBOUNCE_MS = 1000;
const FALLBACK_POLL_MS = 30000;

/**
 * The overview landing page: the federation strip (one column per region),
 * health numbers, warnings with what to do, deployments in progress and the
 * latest events. It loads the summaries once and then refreshes a region
 * when the event stream reports a change there (debounced); while the stream
 * is not connected it polls.
 */
export default class Dashboard extends Component {
  @service token;
  @service system;
  @service frame;
  @service eventStream;
  @service abilities;

  @tracked summaries = [];
  @tracked isLoading = true;
  @tracked loadedAt = null;

  refreshTimers = new Map();
  pollTimer = null;
  members = null;

  get regions() {
    return this.frame.scopedRegions;
  }

  get namespace() {
    return this.frame.namespace;
  }

  get isAllRegions() {
    return this.regions.length > 1;
  }

  get scopeLabel() {
    const region = this.isAllRegions
      ? 'All regions'
      : this.regions[0] || 'Local region';
    const ns =
      this.namespace === '*' ? 'all namespaces' : `namespace ${this.namespace}`;
    return `${region}, ${ns}`;
  }

  get columns() {
    const all = (this.system.regions || []).slice().sort();
    return this.summaries.map((summary) => {
      const position = all.indexOf(summary.region);
      const s = summary;
      return {
        ...s,
        colorIndex: (position < 0 ? 0 : position % 6) + 1,
        label: s.region || 'Local region',
        failedTotal: s.allocations.failed + s.allocations.lost,
        healthy:
          !s.error &&
          !!s.leader &&
          s.servers.alive === s.servers.total &&
          !s.clients.down &&
          !s.seal?.sealed &&
          !s.seal?.needsRecovery,
        allocationSegments: [
          { label: 'running', value: s.allocations.running, color: 'green' },
          { label: 'pending', value: s.allocations.pending, color: 'azure' },
          {
            label: 'failed',
            value: s.allocations.failed + s.allocations.lost,
            color: 'red',
          },
        ],
      };
    });
  }

  get federationStyle() {
    return htmlSafe(`--norca-columns: ${Math.max(1, this.summaries.length)}`);
  }

  get hasAllocations() {
    const a = this.totals.allocations;
    return a.running + a.pending + a.failed + a.lost + a.complete > 0;
  }

  get totals() {
    return totals(this.summaries);
  }

  get warnings() {
    return buildWarnings(this.summaries);
  }

  get deployments() {
    return this.summaries
      .flatMap((s) => s.runningDeployments)
      .sort((a, b) => (b.ModifyIndex || 0) - (a.ModifyIndex || 0))
      .map((d) => ({
        ...d,
        jobRouteId: `${d.JobID}@${d.Namespace || 'default'}`,
        segments: [
          { label: 'healthy', value: d.progress.healthy, color: 'green' },
          { label: 'unhealthy', value: d.progress.unhealthy, color: 'red' },
          {
            label: 'remaining',
            value: Math.max(
              0,
              d.progress.desired - d.progress.healthy - d.progress.unhealthy
            ),
            color: 'secondary',
          },
        ],
      }));
  }

  get events() {
    const regions = new Set(this.regions);
    return this.eventStream.recentEvents
      .filter((e) => !e.Region || regions.has(e.Region))
      .slice(0, 8)
      .map((event) => {
        const item = summarizeEvent(event);
        return {
          ...item,
          time:
            this.isAllRegions && item.region ? item.region : `#${item.index}`,
        };
      });
  }

  get streamState() {
    const states = this.regions.map((r) =>
      this.eventStream.stateFor(r, this.namespace)
    );
    if (states.every((s) => s === 'open')) return 'open';
    if (states.some((s) => s === 'forbidden')) return 'forbidden';
    return states.includes('open') ? 'partial' : 'connecting';
  }

  get allocationChart() {
    const a = this.totals.allocations;
    return {
      tooltip: { trigger: 'item' },
      legend: { bottom: 0, icon: 'circle' },
      series: [
        {
          type: 'pie',
          radius: ['55%', '80%'],
          center: ['50%', '45%'],
          avoidLabelOverlap: true,
          label: { show: false },
          data: [
            { name: 'Running', value: a.running },
            { name: 'Pending', value: a.pending },
            { name: 'Failed', value: a.failed },
            { name: 'Lost', value: a.lost },
            { name: 'Complete', value: a.complete },
          ],
        },
      ],
    };
  }

  get allocationSummaryText() {
    const a = this.totals.allocations;
    return `${a.running} running, ${a.pending} pending, ${a.failed} failed, ${a.lost} lost, ${a.complete} complete`;
  }

  request = (url) => this.token.authorizedRawRequest(url);

  async loadMembers() {
    try {
      const response = await this.request('/v1/agent/members');
      if (!response.ok) return [];
      const json = await response.json();
      return json.Members || [];
    } catch (e) {
      return [];
    }
  }

  async loadRegion(region, members) {
    try {
      return await fetchRegionSummary(this.request, {
        region,
        namespace: this.namespace,
        members,
      });
    } catch (e) {
      return unreachableRegion(region, e);
    }
  }

  @action
  async load() {
    const regions = this.regions;
    if (!regions.length) {
      this.isLoading = false;
      return;
    }
    this.isLoading = !this.summaries.length;
    this.members = await this.loadMembers();
    const summaries = await Promise.all(
      regions.map((r) => this.loadRegion(r, this.members))
    );
    if (this.isDestroying || this.isDestroyed) return;
    this.summaries = summaries;
    this.isLoading = false;
    this.loadedAt = new Date();
  }

  async refreshRegion(region) {
    this.members = await this.loadMembers();
    const fresh = await this.loadRegion(region, this.members);
    if (this.isDestroying || this.isDestroyed) return fresh;
    this.summaries = this.summaries.map((s) =>
      s.region === region ? fresh : s
    );
    return fresh;
  }

  scheduleRefresh(region) {
    if (this.refreshTimers.has(region)) return;
    this.refreshTimers.set(
      region,
      setTimeout(() => {
        this.refreshTimers.delete(region);
        waitForPromise(this.refreshRegion(region));
      }, REFRESH_DEBOUNCE_MS)
    );
  }

  // Loads the data and subscribes to the event stream of every region in
  // scope; runs again when the scope (regions, namespace) changes.
  live = modifier((element, [regions, namespace]) => {
    // Deferred: tracked state must not change while the modifier installs.
    waitForPromise(Promise.resolve().then(() => this.load()));

    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: TOPICS,
        region,
        namespace,
        replay: true,
        onEvents: () => this.scheduleRefresh(region),
        onResync: async () => (await this.refreshRegion(region))?.index || 0,
      })
    );

    if (config.environment !== 'test') {
      this.pollTimer = setInterval(() => {
        if (this.streamState !== 'open') waitForPromise(this.load());
      }, FALLBACK_POLL_MS);
    }

    return () => {
      unsubscribers.forEach((unsubscribe) => unsubscribe());
      clearInterval(this.pollTimer);
      this.refreshTimers.forEach((timer) => clearTimeout(timer));
      this.refreshTimers.clear();
    };
  });
}
