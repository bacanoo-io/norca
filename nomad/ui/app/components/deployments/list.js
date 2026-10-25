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
  listDeployments,
  progressSegments,
  ACTIVE,
} from 'nomad-ui/utils/deployments';

const POLL_MS = 30000;

/**
 * Deployments across jobs and the regions in scope, newest first, with the
 * multiregion rollouts in progress shown per region on top.
 */
export default class DeploymentsList extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked regions = [];
  @tracked isLoading = true;
  @tracked showAll = false;

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

  get all() {
    return this.regions
      .flatMap((r) =>
        (r.deployments || []).map((d) => ({
          ...d,
          region: r.region,
          shortId: d.ID.split('-')[0],
          jobRouteId: `${d.JobID}@${d.Namespace || 'default'}`,
          isActive: ACTIVE.includes(d.Status),
          progress: progressSegments(d),
        }))
      )
      .sort((a, b) => (b.CreateIndex || 0) - (a.CreateIndex || 0));
  }

  get rows() {
    return this.showAll ? this.all : this.all.filter((d) => d.isActive);
  }

  // One entry per multiregion rollout in progress (namespace, job,
  // version). A rollout stays on the page once shown: when the region in
  // scope finishes, the other regions may still be rolling out.
  @tracked shownRollouts = [];

  get rollouts() {
    return this.shownRollouts;
  }

  rememberRollouts() {
    const seen = new Map(this.shownRollouts.map((r) => [r.key, r]));
    for (const d of this.all) {
      if (!d.IsMultiregion || !d.isActive) continue;
      const key = `${d.Namespace}/${d.JobID}/${d.JobVersion}`;
      if (!seen.has(key))
        seen.set(key, {
          key,
          job: d.JobID,
          namespace: d.Namespace || 'default',
          version: d.JobVersion,
          homeRegion: d.region,
        });
    }
    if (seen.size !== this.shownRollouts.length)
      this.shownRollouts = [...seen.values()];
  }

  get columns() {
    const columns = [
      { key: 'job', label: 'Job' },
      { key: 'version', label: 'Version' },
    ];
    if (this.isAllRegions) columns.push({ key: 'region', label: 'Region' });
    columns.push(
      { key: 'status', label: 'Status' },
      { key: 'progress', label: 'Healthy' },
      { key: 'description', label: 'Description' }
    );
    return columns;
  }

  get errors() {
    return this.regions.filter((r) => r.error);
  }

  async loadRegion(region) {
    try {
      const { body, index } = await listDeployments(
        this.token,
        region,
        this.namespace
      );
      return { region, deployments: body || [], index };
    } catch (e) {
      return {
        region,
        deployments: [],
        error:
          e.status === 403
            ? 'Your token cannot read the deployments here (read-job is needed).'
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
    this.rememberRollouts();
    this.isLoading = false;
  }

  async refreshRegion(region) {
    const fresh = await this.loadRegion(region);
    if (this.isDestroying || this.isDestroyed) return fresh;
    this.regions = this.regions.map((r) => (r.region === region ? fresh : r));
    this.rememberRollouts();
    return fresh;
  }

  setup = modifier((element, [regions, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const timers = new Map();
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: ['Deployment'],
        region,
        namespace,
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

  @action
  setShowAll(value) {
    this.showAll = value;
  }

  @action
  refresh() {
    return this.load();
  }
}
