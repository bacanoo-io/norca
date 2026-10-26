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
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  listPolicies,
  getPolicy,
  jobScale,
  eventItem,
  decisionItem,
} from 'nomad-ui/utils/scaling';
import LocalList from 'nomad-ui/utils/local-list';

const POLL_MS = 30000;
const LIVE_LIMIT = 30;

/**
 * The scaling overview: every scaling policy of the regions and namespaces
 * in scope with its engine (built-in or external), bounds, the current
 * count and the last decision, and a decision timeline from the scaling
 * events of the groups and the live decisions of the built-in engine.
 */
export default class ScalingList extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked regions = [];
  @tracked live = [];
  @tracked isLoading = true;
  @tracked engineFilter = '';

  get scopedRegions() {
    return this.frame.scopedRegions;
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get isAllRegions() {
    return this.scopedRegions.length > 1;
  }

  get scopeLabel() {
    const region = this.isAllRegions
      ? 'All regions'
      : this.scopedRegions[0] || 'Local region';
    const ns =
      this.namespace === '*' ? 'all namespaces' : `namespace ${this.namespace}`;
    return `${region}, ${ns}`;
  }

  get engineOptions() {
    return [
      { value: '', label: 'All engines' },
      { value: 'builtin', label: 'Built-in' },
      { value: 'external', label: 'External' },
    ];
  }

  get columns() {
    const columns = [
      { key: 'target', label: 'Job / group', sortable: true, hideable: false },
    ];
    if (this.isAllRegions)
      columns.push({ key: 'region', label: 'Region', sortable: true });
    columns.push(
      { key: 'engine', label: 'Engine', sortable: true },
      { key: 'Min', label: 'Min–max', sortable: true },
      { key: 'current', label: 'Current' },
      { key: 'lastTime', label: 'Last decision', sortable: true },
      { key: 'details', label: 'Policy', srOnly: true, hideable: false }
    );
    return columns;
  }

  list = new LocalList({
    sortBy: 'target',
    searchProps: ['target', 'namespace', 'region'],
  });

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get rows() {
    return this.regions
      .flatMap((r) => r.rows || [])
      .filter((row) => !this.engineFilter || row.engine === this.engineFilter);
  }

  get forbidden() {
    return this.regions.length && this.regions.every((r) => r.forbidden);
  }

  get errors() {
    return this.regions.filter((r) => r.error);
  }

  get timeline() {
    const persisted = this.regions.flatMap((r) => r.events || []);
    return [...this.live, ...persisted]
      .sort((a, b) => b.timestamp - a.timestamp)
      .slice(0, 25);
  }

  get counts() {
    const rows = this.regions.flatMap((r) => r.rows || []);
    return {
      builtin: rows.filter((r) => r.engine === 'builtin').length,
      external: rows.filter((r) => r.engine === 'external').length,
      disabled: rows.filter((r) => !r.Enabled).length,
    };
  }

  async loadRegion(region) {
    const result = { region, rows: [], events: [] };
    let stubs;
    let builtin;
    try {
      const [all, b] = await Promise.all([
        listPolicies(this.token, region, { namespace: this.namespace }),
        listPolicies(this.token, region, {
          namespace: this.namespace,
          engine: 'builtin',
        }),
      ]);
      stubs = all.body || [];
      builtin = new Set((b.body || []).map((p) => p.ID));
      result.index = all.index;
    } catch (e) {
      if (e.status === 403) result.forbidden = true;
      else result.error = errorMessage(e);
      return result;
    }

    const jobs = new Map();
    for (const s of stubs) {
      const key = `${s.Target?.Namespace || 'default'}/${s.Target?.Job}`;
      if (!jobs.has(key)) jobs.set(key, s.Target);
    }
    const scales = new Map();
    await Promise.all([
      ...[...jobs.entries()].map(async ([key, target]) => {
        try {
          const { body } = await jobScale(
            this.token,
            region,
            target.Job,
            target.Namespace || 'default'
          );
          scales.set(key, body);
        } catch (e) {
          scales.set(key, null);
        }
      }),
    ]);
    const details = await Promise.all(
      stubs.map(async (s) => {
        try {
          const { body } = await getPolicy(
            this.token,
            region,
            s.ID,
            s.Target?.Namespace || 'default'
          );
          return body;
        } catch (e) {
          return s;
        }
      })
    );

    result.rows = details.map((p) => {
      const t = p.Target || {};
      const ns = t.Namespace || 'default';
      const scale = scales.get(`${ns}/${t.Job}`);
      const group = scale?.TaskGroups?.[t.Group];
      const ctx = { job: t.Job, group: t.Group, namespace: ns, region };
      const events = (group?.Events || []).map((e) => eventItem(e, ctx));
      result.events.push(...events);
      const last = events.find((e) => e.count !== null) || events[0];
      return {
        ...p,
        region,
        namespace: ns,
        jobRouteId: `${t.Job}@${ns}`,
        engine: builtin.has(p.ID) ? 'builtin' : 'external',
        bounds: p.Min !== undefined ? `${p.Min}–${p.Max}` : '–',
        current: group ? `${group.Running} of ${group.Desired}` : '–',
        last,
        target: `${t.Job} / ${t.Group}`,
        lastTime: last?.timestamp,
      };
    });
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

  setup = modifier((element, [regions, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const timers = new Map();
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: ['Scaling', 'Job'],
        region,
        namespace,
        onEvents: (events) => {
          const decisions = events
            .filter((e) => e.Topic === 'Scaling')
            .map(decisionItem);
          if (decisions.length)
            this.live = [...decisions.reverse(), ...this.live].slice(
              0,
              LIVE_LIMIT
            );
          if (timers.has(region)) return;
          timers.set(
            region,
            setTimeout(() => {
              timers.delete(region);
              waitForPromise(this.refreshRegion(region));
            }, 2000)
          );
        },
        onResync: async () => (await this.refreshRegion(region))?.index || 0,
      })
    );
    let poll = null;
    if (config.environment !== 'test') {
      poll = setInterval(() => {
        if (
          regions.some(
            (r) => this.eventStream.stateFor(r, namespace) !== 'open'
          )
        )
          waitForPromise(this.load());
      }, POLL_MS);
    }
    return () => {
      unsubscribers.forEach((u) => u());
      timers.forEach((t) => clearTimeout(t));
      clearInterval(poll);
    };
  });

  setEngine = (value) => {
    this.engineFilter = value;
  };
}
