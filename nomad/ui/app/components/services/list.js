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
  listServices,
  serviceInstances,
  healthSummary,
  mtlsMode,
} from 'nomad-ui/utils/networking';
import LocalList from 'nomad-ui/utils/local-list';

const POLL_MS = 30000;

/**
 * The native service catalog: every service of the regions and namespaces
 * in scope with the health of its instances per region (passing, warning,
 * critical, and how many take part in discovery), tags and mTLS mode.
 * Live from the Service topic.
 */
export default class ServicesList extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked regions = [];
  @tracked roots = null;
  @tracked isLoading = true;

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

  get columns() {
    const columns = [
      { key: 'name', label: 'Service', sortable: true, hideable: false },
      { key: 'namespace', label: 'Namespace', sortable: true },
    ];
    for (const r of this.regions)
      columns.push({
        key: `region-${r.region}`,
        label: this.isAllRegions ? r.region : 'Healthy',
        sortable: true,
      });
    columns.push(
      { key: 'tags', label: 'Tags' },
      { key: 'mtls', label: 'mTLS', sortable: true }
    );
    return columns;
  }

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'namespace', 'tagText'],
  });

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get rows() {
    const rows = new Map();
    for (const r of this.regions) {
      for (const s of r.services || []) {
        const key = `${s.namespace}/${s.name}`;
        if (!rows.has(key))
          rows.set(key, {
            key,
            name: s.name,
            namespace: s.namespace,
            tags: new Set(),
            mtls: new Set(),
            byRegion: {},
            external: false,
          });
        const row = rows.get(key);
        s.tags.forEach((t) => row.tags.add(t));
        if (s.mtls) row.mtls.add(s.mtls);
        if (s.external) row.external = true;
        row.byRegion[r.region] = s.health;
      }
    }
    return [...rows.values()]
      .map((row) => {
        const cells = this.regions.map((r) => ({
          key: `region-${r.region}`,
          health: row.byRegion[r.region] || null,
        }));
        return {
          ...row,
          ...Object.fromEntries(
            cells.map((c) => [c.key, c.health ? c.health.passing : null])
          ),
          tags: [...row.tags],
          tagText: [...row.tags].join(' '),
          mtls: [...row.mtls].join(', '),
          cells,
        };
      })
      .sort((a, b) =>
        a.name === b.name
          ? a.namespace.localeCompare(b.namespace)
          : a.name.localeCompare(b.name)
      );
  }

  get totals() {
    const t = { services: 0, instances: 0, passing: 0, excluded: 0 };
    for (const r of this.regions)
      for (const s of r.services || []) {
        t.services++;
        t.instances += s.health.total;
        t.passing += s.health.passing;
        t.excluded += s.health.total - s.health.discoverable;
      }
    return t;
  }

  get activeRoot() {
    const roots = this.roots?.Roots || [];
    return roots.find((r) => r.ID === this.roots.ActiveRootID) || null;
  }

  get errors() {
    return this.regions.filter((r) => r.error);
  }

  async loadRegion(region) {
    const result = { region, services: [] };
    try {
      const { body, index } = await listServices(
        this.token,
        region,
        this.namespace
      );
      result.index = index;
      const names = (body || []).flatMap((ns) =>
        (ns.Services || []).map((s) => ({
          namespace: ns.Namespace,
          name: s.ServiceName,
          tags: s.Tags || [],
        }))
      );
      result.services = await Promise.all(
        names.map(async (s) => {
          let instances = [];
          try {
            instances =
              (await serviceInstances(this.token, region, s.name, s.namespace))
                .body || [];
          } catch (e) {
            instances = [];
          }
          return {
            ...s,
            health: healthSummary(instances),
            mtls: mtlsMode(instances),
            external: instances.some((i) => i.External),
          };
        })
      );
    } catch (e) {
      result.error =
        e.status === 403
          ? 'Your token cannot read services here (read-job is needed).'
          : errorMessage(e);
    }
    return result;
  }

  async load() {
    const [regions, roots] = await Promise.all([
      Promise.all(this.scopedRegions.map((r) => this.loadRegion(r))),
      apiRequest(this.token, '/v1/mtls/roots', {
        params: { region: this.scopedRegions[0] },
      })
        .then((r) => r.body)
        .catch(() => null),
    ]);
    if (this.isDestroying || this.isDestroyed) return;
    this.regions = regions;
    this.roots = roots;
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
        topics: ['Service'],
        region,
        namespace,
        onEvents: () => {
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
}
