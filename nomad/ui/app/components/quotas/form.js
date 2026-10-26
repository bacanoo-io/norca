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
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { DIMENSIONS, getQuota, saveQuota } from 'nomad-ui/utils/quotas';

const NAME = /^[a-zA-Z0-9-_]{1,128}$/;

function toField(value) {
  return value === undefined || value === null || value === 0
    ? ''
    : String(value);
}

function toNumber(value) {
  const v = String(value ?? '').trim();
  return v === '' ? 0 : Number(v);
}

/**
 * Creating or editing a quota: name, description, enforcement, one limit
 * per region (CPU, memory, memory max, allocations, Variables; empty means
 * unlimited, -1 disallowed) and the task defaults and maximums.
 */
export default class QuotasForm extends Component {
  @service token;
  @service system;
  @service router;
  @service notifications;

  @tracked name = '';
  @tracked description = '';
  @tracked enforce = 'schedule';
  @tracked limits = [];
  @tracked taskMaxCPU = '';
  @tracked taskMaxMemory = '';
  @tracked taskDefaultCPU = '';
  @tracked taskDefaultMemory = '';
  @tracked original = null;
  @tracked isLoading = !!this.args.name;
  @tracked isSaving = false;
  @tracked error = null;
  @tracked submitted = false;

  dimensions = DIMENSIONS;

  get isNew() {
    return !this.args.name;
  }

  get region() {
    return this.system.activeRegion || '';
  }

  get regionOptions() {
    const regions = (this.system.regions || []).slice().sort();
    return regions.length ? regions : [this.region || 'global'];
  }

  get nameError() {
    if (!this.submitted || !this.isNew) return null;
    if (!NAME.test(this.name))
      return 'Use 1 to 128 letters, digits, dashes and underscores.';
    return null;
  }

  get limitErrors() {
    const regions = this.limits.map((l) => l.region);
    return regions.length !== new Set(regions).size
      ? 'Each region can have one limit only.'
      : null;
  }

  get enforceOptions() {
    return [
      { value: 'schedule', label: 'When scheduling (jobs wait, blocked)' },
      { value: 'submit', label: 'At job submission (rejected)' },
    ];
  }

  emptyLimit(region) {
    const limit = { key: `${Date.now()}-${Math.random()}`, region };
    DIMENSIONS.forEach((d) => (limit[d.id] = ''));
    return limit;
  }

  async load() {
    if (this.isNew) {
      this.limits = [this.emptyLimit(this.regionOptions[0])];
      return;
    }
    try {
      const { body } = await getQuota(this.token, this.region, this.args.name);
      this.original = body;
      this.name = body.Name;
      this.description = body.Description || '';
      this.enforce = body.Enforce || 'schedule';
      this.limits = (body.Limits || []).map((l) => {
        const limit = this.emptyLimit(l.Region);
        DIMENSIONS.forEach((d) => (limit[d.id] = toField(d.get(l))));
        return limit;
      });
      this.taskMaxCPU = toField(body.TaskMax?.CPU);
      this.taskMaxMemory = toField(body.TaskMax?.MemoryMB);
      this.taskDefaultCPU = toField(body.TaskDefaults?.CPU);
      this.taskDefaultMemory = toField(body.TaskDefaults?.MemoryMB);
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier(() => {
    waitForPromise(Promise.resolve().then(() => this.load()));
  });

  @action set(field, value) {
    this[field] = value;
  }

  @action setLimit(limit, field, value) {
    this.limits = this.limits.map((l) =>
      l.key === limit.key ? { ...l, [field]: value } : l
    );
  }

  @action addLimit() {
    const used = new Set(this.limits.map((l) => l.region));
    const next =
      this.regionOptions.find((r) => !used.has(r)) || this.regionOptions[0];
    this.limits = [...this.limits, this.emptyLimit(next)];
  }

  @action removeLimit(limit) {
    this.limits = this.limits.filter((l) => l.key !== limit.key);
  }

  buildSpec() {
    const resources = (cpu, memory) => {
      const r = {};
      if (toNumber(cpu)) r.CPU = toNumber(cpu);
      if (toNumber(memory)) r.MemoryMB = toNumber(memory);
      return Object.keys(r).length ? r : null;
    };
    return {
      ...(this.original || {}),
      Name: this.name.trim(),
      Description: this.description.trim(),
      Enforce: this.enforce,
      Limits: this.limits.map((l) => {
        const limit = { Region: l.region, RegionLimit: {} };
        DIMENSIONS.forEach((d) => d.set(limit, toNumber(l[d.id])));
        return limit;
      }),
      TaskMax: resources(this.taskMaxCPU, this.taskMaxMemory),
      TaskDefaults: resources(this.taskDefaultCPU, this.taskDefaultMemory),
    };
  }

  @action
  async save(event) {
    event.preventDefault();
    this.submitted = true;
    if (this.nameError || this.limitErrors) return;
    this.isSaving = true;
    this.error = null;
    try {
      const spec = this.buildSpec();
      await saveQuota(this.token, this.region, spec);
      this.notifications.add({
        title: `Quota ${spec.Name} saved`,
        color: 'success',
      });
      this.router.transitionTo('quotas.quota', spec.Name);
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.isSaving = false;
    }
  }
}
