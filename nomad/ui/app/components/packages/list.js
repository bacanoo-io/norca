/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  listReleases,
  isDrifted,
  signature,
  phaseTone,
  RELEASE_KIND,
} from 'nomad-ui/utils/releases';

const POLL_MS = 30000;

/**
 * The installed packages: the pack releases of the regions and namespaces
 * in scope with pack and version, revision, phase, drift, regions and
 * signature. Installing and upgrading are done with the norca pack CLI,
 * which renders the pack.
 */
export default class PackagesList extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked regions = [];
  @tracked isLoading = true;
  @tracked filter = '';

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
      { key: 'release', label: 'Release' },
      { key: 'pack', label: 'Pack' },
      { key: 'revision', label: 'Revision' },
      { key: 'phase', label: 'Status' },
      { key: 'drift', label: 'Drift' },
      { key: 'regions', label: 'Regions' },
      { key: 'signature', label: 'Signature' },
    ];
    return columns;
  }

  get rows() {
    const q = this.filter.trim().toLowerCase();
    return this.regions
      .flatMap((r) =>
        (r.releases || []).map((rel) => {
          const pack = rel.Spec?.Pack || {};
          const multi = rel.Spec?.Multiregion?.Regions || [];
          return {
            ...rel,
            region: r.region,
            pack: `${pack.Name || '?'} ${pack.Version || ''}`.trim(),
            phase: rel.Status?.Phase || 'pending',
            phaseTone: phaseTone(rel.Status?.Phase),
            drifted: isDrifted(rel),
            signature: signature(rel),
            regionList: multi.length ? multi : [r.region || 'local'],
          };
        })
      )
      .filter(
        (row) =>
          !q ||
          row.Name.toLowerCase().includes(q) ||
          row.pack.toLowerCase().includes(q)
      )
      .sort((a, b) => a.Name.localeCompare(b.Name));
  }

  get errors() {
    return this.regions.filter((r) => r.error);
  }

  get driftCount() {
    return this.rows.filter((r) => r.drifted).length;
  }

  async loadRegion(region) {
    try {
      const { body, index } = await listReleases(
        this.token,
        region,
        this.namespace
      );
      return { region, releases: body || [], index };
    } catch (e) {
      return {
        region,
        releases: [],
        error:
          e.status === 403
            ? `Your token cannot read releases here: it needs the read capability of object ${RELEASE_KIND}.`
            : errorMessage(e),
      };
    }
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
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: [`CustomObject:${RELEASE_KIND}`],
        region,
        namespace,
        onEvents: () => waitForPromise(this.refreshRegion(region)),
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
      clearInterval(poll);
    };
  });

  @action
  setFilter(value) {
    this.filter = value;
  }
}
