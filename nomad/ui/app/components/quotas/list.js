/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import {
  listQuotas,
  listUsages,
  limitFor,
  usedFor,
  usageRows,
  quotaBlocked,
} from 'nomad-ui/utils/quotas';

const POLL_MS = 30000;

/**
 * The quotas page: per quota and region in scope, usage bars of every
 * limited dimension, the namespaces that use the quota, and the
 * evaluations blocked by a quota (they run again automatically when the
 * quota frees up). Live from the Quota and Evaluation topics.
 */
export default class QuotasList extends Component {
  @service token;
  @service frame;
  @service eventStream;
  @service abilities;

  @tracked regions = [];
  @tracked isLoading = true;

  get scopedRegions() {
    return this.frame.scopedRegions;
  }

  get isAllRegions() {
    return this.scopedRegions.length > 1;
  }

  get scopeLabel() {
    return this.isAllRegions
      ? 'All regions'
      : this.scopedRegions[0] || 'Local region';
  }

  get canWrite() {
    return this.abilities.can('write quota-spec');
  }

  get forbidden() {
    return this.regions.length && this.regions.every((r) => r.forbidden);
  }

  get errors() {
    return this.regions.filter((r) => r.error);
  }

  get cards() {
    const cards = [];
    for (const r of this.regions) {
      for (const spec of r.specs || []) {
        const limit = limitFor(spec, r.region);
        if (!limit) continue;
        const usage = (r.usages || []).find((u) => u.Name === spec.Name);
        const rows = usageRows(limit, usedFor(usage, limit));
        cards.push({
          key: `${spec.Name}|${r.region}`,
          name: spec.Name,
          region: r.region,
          title: this.isAllRegions ? `${spec.Name} (${r.region})` : spec.Name,
          description: spec.Description,
          enforce: spec.Enforce,
          rows,
          nearlyFull: rows.some(
            (x) => x.state === 'nearly' || x.state === 'full'
          ),
          namespaces: (r.namespaces || [])
            .filter((n) => n.Quota === spec.Name)
            .map((n) => n.Name),
        });
      }
    }
    return cards;
  }

  get specsWithoutLimit() {
    const shown = new Set(this.cards.map((c) => c.name));
    const names = new Set();
    for (const r of this.regions)
      for (const spec of r.specs || [])
        if (!shown.has(spec.Name)) names.add(spec.Name);
    return [...names];
  }

  get blocked() {
    return this.regions.flatMap((r) =>
      quotaBlocked(r.evaluations).map((e) => ({
        ...e,
        region: r.region,
        jobRouteId: `${e.JobID}@${e.Namespace || 'default'}`,
        since: e.CreateTime ? new Date(e.CreateTime / 1e6) : null,
        dimensionText: e.dimensions.join(', ') || 'unknown',
      }))
    );
  }

  get blockedColumns() {
    const columns = [{ key: 'job', label: 'Job' }];
    if (this.isAllRegions) columns.push({ key: 'region', label: 'Region' });
    columns.push(
      { key: 'quota', label: 'Quota' },
      { key: 'exhausted', label: 'Exhausted' },
      { key: 'since', label: 'Waiting since' }
    );
    return columns;
  }

  async optional(promise) {
    try {
      return (await promise).body;
    } catch (e) {
      return null;
    }
  }

  async loadRegion(region) {
    const result = { region };
    try {
      const [specs, usages] = await Promise.all([
        listQuotas(this.token, region),
        listUsages(this.token, region),
      ]);
      result.specs = specs.body || [];
      result.usages = usages.body || [];
      result.index = Math.max(specs.index, usages.index);
    } catch (e) {
      if (e.status === 403) result.forbidden = true;
      else result.error = errorMessage(e);
      return result;
    }
    const [namespaces, evaluations] = await Promise.all([
      this.optional(
        apiRequest(this.token, '/v1/namespaces', { params: { region } })
      ),
      this.optional(
        apiRequest(this.token, '/v1/evaluations', {
          params: {
            region,
            namespace: '*',
            filter: 'Status == "blocked" and QuotaLimitReached != ""',
          },
        })
      ),
    ]);
    result.namespaces = namespaces || [];
    result.evaluations = evaluations || [];
    return result;
  }

  async load() {
    const regions = await Promise.all(
      this.scopedRegions.map((r) => this.loadRegion(r))
    );
    if (this.isDestroying || this.isDestroyed) return;
    this.regions = regions;
    this.isLoading = false;
  }

  async refreshRegion(region) {
    const fresh = await this.loadRegion(region);
    if (this.isDestroying || this.isDestroyed) return fresh;
    this.regions = this.regions.map((r) => (r.region === region ? fresh : r));
    return fresh;
  }

  live = modifier((element, [regions]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const timers = new Map();
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: ['Quota', 'Evaluation'],
        region,
        onEvents: () => {
          if (timers.has(region)) return;
          timers.set(
            region,
            setTimeout(() => {
              timers.delete(region);
              waitForPromise(this.refreshRegion(region));
            }, 1000)
          );
        },
        onResync: async () => (await this.refreshRegion(region))?.index || 0,
      })
    );
    let poll = null;
    if (config.environment !== 'test') {
      poll = setInterval(() => {
        if (regions.some((r) => this.eventStream.stateFor(r, '*') !== 'open'))
          waitForPromise(this.load());
      }, POLL_MS);
    }
    return () => {
      unsubscribers.forEach((u) => u());
      timers.forEach((t) => clearTimeout(t));
      clearInterval(poll);
    };
  });
}
