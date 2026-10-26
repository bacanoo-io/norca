/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';

const REFRESH_DEBOUNCE_MS = 500;
const FALLBACK_POLL_MS = 10000;
const ACTIVE = [
  'pending',
  'blocked',
  'unblocking',
  'running',
  'paused',
  'initializing',
];

/**
 * The per-region timeline of a multiregion deployment (UI plan, job
 * details: Deployments): the latest deployment of the job in each region
 * of its multiregion block, in rollout order, with the Run, Unblock and
 * Fail operations of the regions. The regions coordinate over Nomad's
 * cross-region RPC; the UI only reads and calls the deployment endpoints of
 * each region.
 *
 * Live: the Deployment topic of the event stream of each region refreshes
 * that region; without a stream the view polls every 10 seconds.
 */
export default class JobsMultiregionTimeline extends Component {
  @service token;
  @service eventStream;
  @service notifications;

  @tracked regions = [];
  @tracked isLoading = true;
  @tracked actionError = null;

  timers = new Map();

  get job() {
    return this.args.job;
  }

  get strategy() {
    return this.job.multiregion?.Strategy || {};
  }

  get namespace() {
    return this.job.get('namespace.name') || 'default';
  }

  get streamOpen() {
    return this.regions.every(
      (r) => this.eventStream.stateFor(r.name, this.namespace) === 'open'
    );
  }

  async fetchRegion(name) {
    const id = encodeURIComponent(this.job.plainId);
    const url = `/v1/job/${id}/deployment?region=${encodeURIComponent(
      name
    )}&namespace=${encodeURIComponent(this.namespace)}`;
    try {
      const response = await this.token.authorizedRawRequest(url);
      if (!response.ok) {
        return { name, error: `${response.status} ${response.statusText}` };
      }
      const deployment = await response.json();
      return { name, deployment: deployment ? summarize(deployment) : null };
    } catch (e) {
      return { name, error: 'The region cannot be reached.' };
    }
  }

  async load() {
    const names = this.job.multiregionRegions || [];
    const results = await Promise.all(names.map((n) => this.fetchRegion(n)));
    if (this.isDestroying || this.isDestroyed) return;
    this.regions = results.map((r, index) => ({
      ...r,
      colorIndex: (index % 6) + 1,
    }));
    this.isLoading = false;
  }

  async refreshRegion(name) {
    const fresh = await this.fetchRegion(name);
    if (this.isDestroying || this.isDestroyed) return;
    this.regions = this.regions.map((r) =>
      r.name === name ? { ...fresh, colorIndex: r.colorIndex } : r
    );
  }

  scheduleRefresh(name) {
    if (this.timers.has(name)) return;
    this.timers.set(
      name,
      setTimeout(() => {
        this.timers.delete(name);
        waitForPromise(this.refreshRegion(name));
      }, REFRESH_DEBOUNCE_MS)
    );
  }

  live = modifier(() => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const names = this.job.multiregionRegions || [];
    const unsubscribers = names.map((region) =>
      this.eventStream.subscribe({
        // Deployment events carry the job ID among their filter keys.
        topics: [`Deployment:${this.job.plainId}`],
        region,
        namespace: this.namespace,
        onEvents: () => this.scheduleRefresh(region),
        onResync: () => this.refreshRegion(region).then(() => 0),
      })
    );
    let poll = null;
    if (config.environment !== 'test') {
      poll = setInterval(() => {
        if (!this.streamOpen) waitForPromise(this.load());
      }, FALLBACK_POLL_MS);
    }
    return () => {
      unsubscribers.forEach((u) => u());
      clearInterval(poll);
      this.timers.forEach((t) => clearTimeout(t));
      this.timers.clear();
    };
  });

  @action
  async operate(region, operation) {
    this.actionError = null;
    const deployment = region.deployment;
    const url = `/v1/deployment/${operation}/${
      deployment.id
    }?region=${encodeURIComponent(region.name)}&namespace=${encodeURIComponent(
      this.namespace
    )}`;
    try {
      const response = await this.token.authorizedRawRequest(url, {
        method: 'POST',
        body: JSON.stringify({ DeploymentID: deployment.id }),
      });
      if (!response.ok) {
        const text = await response.text();
        throw new Error(text || response.statusText);
      }
      this.notifications.add({
        title: `Deployment ${
          operation === 'fail'
            ? 'failed'
            : operation === 'run'
            ? 'started'
            : 'unblocked'
        }`,
        message: `${this.job.name} in ${region.name}`,
        color: 'success',
      });
      await this.refreshRegion(region.name);
    } catch (err) {
      this.actionError = {
        region: region.name,
        message: messageFromAdapterError(err, 'change deployments'),
      };
    }
  }
}

function summarize(deployment) {
  const groups = Object.values(deployment.TaskGroups || {});
  const healthy = groups.reduce((s, g) => s + (g.HealthyAllocs || 0), 0);
  const desired = groups.reduce((s, g) => s + (g.DesiredTotal || 0), 0);
  const status = deployment.Status;
  return {
    id: deployment.ID,
    shortId: String(deployment.ID).split('-')[0],
    status,
    description: deployment.StatusDescription,
    version: deployment.JobVersion,
    isMultiregion: deployment.IsMultiregion,
    healthy,
    desired,
    percent: desired ? Math.round((healthy / desired) * 100) : 0,
    isActive: ACTIVE.includes(status),
    canRun: status === 'pending',
    canUnblock: status === 'blocked',
    canFail: ACTIVE.includes(status),
  };
}
