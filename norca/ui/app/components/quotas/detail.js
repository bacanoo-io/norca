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
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getQuota,
  deleteQuota,
  usedFor,
  usageRows,
  quotaBlocked,
} from 'nomad-ui/utils/quotas';

/**
 * A quota: its limits with the usage of every region it limits, the task
 * defaults and maximums, the namespaces that use it and the evaluations it
 * blocks. Edit and delete for tokens with quota write.
 */
export default class QuotasDetail extends Component {
  @service token;
  @service system;
  @service router;
  @service eventStream;
  @service abilities;
  @service notifications;

  @tracked spec = null;
  @tracked usages = {};
  @tracked namespaces = [];
  @tracked evaluations = [];
  @tracked isLoading = true;
  @tracked notFound = false;
  @tracked loadError = null;
  @tracked confirmingDelete = false;
  @tracked isDeleting = false;
  @tracked deleteError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get canWrite() {
    return this.abilities.can('write quota-spec');
  }

  get limits() {
    return (this.spec?.Limits || []).map((limit) => {
      const usage = this.usages[limit.Region];
      return {
        region: limit.Region,
        rows: usageRows(limit, usedFor(usage, limit)),
        unavailable: usage === null,
      };
    });
  }

  get taskLimits() {
    const fmt = (r) => {
      if (!r) return 'none';
      const parts = [];
      if (r.CPU) parts.push(`${r.CPU} MHz CPU`);
      if (r.Cores) parts.push(`${r.Cores} cores`);
      if (r.MemoryMB) parts.push(`${r.MemoryMB} MB memory`);
      if (r.MemoryMaxMB) parts.push(`${r.MemoryMaxMB} MB memory max`);
      return parts.join(', ') || 'none';
    };
    return [
      {
        title: 'Enforced at',
        value:
          this.spec?.Enforce === 'submit' ? 'job submission' : 'scheduling',
      },
      { title: 'Task defaults', value: fmt(this.spec?.TaskDefaults) },
      { title: 'Task maximum', value: fmt(this.spec?.TaskMax) },
    ];
  }

  get usedBy() {
    return this.namespaces.filter((n) => n.Quota === this.args.name);
  }

  get blocked() {
    return quotaBlocked(this.evaluations)
      .filter((e) => e.QuotaLimitReached === this.args.name)
      .map((e) => ({
        ...e,
        jobRouteId: `${e.JobID}@${e.Namespace || 'default'}`,
        dimensionText: e.dimensions.join(', ') || 'unknown',
      }));
  }

  async optional(promise) {
    try {
      return (await promise).body;
    } catch (e) {
      return null;
    }
  }

  async load() {
    try {
      const { body } = await getQuota(this.token, this.region, this.args.name);
      this.spec = body;
      this.notFound = false;
    } catch (e) {
      if (e.status === 404) this.notFound = true;
      else this.loadError = errorMessage(e);
      this.isLoading = false;
      return;
    }
    const usages = {};
    await Promise.all(
      (this.spec.Limits || []).map(async (limit) => {
        try {
          const { body } = await apiRequest(
            this.token,
            `/v1/quota/usage/${encodeURIComponent(this.args.name)}`,
            { params: { region: limit.Region } }
          );
          usages[limit.Region] = body;
        } catch (e) {
          // a new quota has no usage yet; other failures: unavailable
          usages[limit.Region] = e.status === 404 ? { Used: {} } : null;
        }
      })
    );
    const [namespaces, evaluations] = await Promise.all([
      this.optional(
        apiRequest(this.token, '/v1/namespaces', {
          params: { region: this.region },
        })
      ),
      this.optional(
        apiRequest(this.token, '/v1/evaluations', {
          params: {
            region: this.region,
            namespace: '*',
            filter: 'Status == "blocked" and QuotaLimitReached != ""',
          },
        })
      ),
    ]);
    if (this.isDestroying || this.isDestroyed) return;
    this.usages = usages;
    this.namespaces = namespaces || [];
    this.evaluations = evaluations || [];
    this.isLoading = false;
  }

  live = modifier((element, [name, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`Quota:${name}`],
      region,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  @action
  askDelete() {
    this.deleteError = null;
    this.confirmingDelete = true;
  }

  @action
  cancelDelete() {
    this.confirmingDelete = false;
  }

  @action
  async deleteQuota() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deleteQuota(this.token, this.region, this.args.name);
      this.notifications.add({
        title: `Quota ${this.args.name} deleted`,
        color: 'success',
      });
      this.confirmingDelete = false;
      this.router.transitionTo('quotas.index');
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }
}
