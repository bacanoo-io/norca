/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';

// The scaling block of a task group in the JSON form of the API.
export const SCALING_SCHEMA = {
  type: 'object',
  required: ['Max'],
  properties: {
    Min: { type: 'integer', minimum: 0 },
    Max: { type: 'integer', minimum: 0 },
    Enabled: { type: 'boolean' },
    Type: { type: 'string', enum: ['horizontal', ''] },
    Engine: { type: 'string', enum: ['builtin', 'external', ''] },
    Policy: { type: ['object', 'null'] },
    Target: { type: ['object', 'null'] },
  },
};

const TEMPLATE = {
  Min: 1,
  Max: 3,
  Enabled: true,
  Policy: {
    evaluation_interval: '30s',
    cooldown: '2m',
    check: {
      cpu: {
        source: 'nomad-apm',
        query: 'avg_cpu-allocated',
        strategy: { 'target-value': { target: 70 } },
      },
    },
  },
};

async function check(response) {
  if (response.ok) return response.json();
  const text = await response.text().catch(() => '');
  const error = new Error(text.trim() || response.statusText);
  error.errors = [{ status: String(response.status), detail: text.trim() }];
  throw error;
}

/**
 * The editor of the scaling block of a task group: JSON with the schema of
 * the block, previewed as a job plan before the job is submitted with the
 * new block (UI plan: unified editor, Scaling policy editor).
 */
export default class JobsScalingPolicyEditor extends Component {
  @service token;
  @service notifications;

  @tracked text = '';
  @tracked original = '';
  @tracked loadError = null;

  definition = null;
  schema = SCALING_SCHEMA;

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  get job() {
    return this.args.job;
  }

  get url() {
    return `/v1/job/${encodeURIComponent(this.job.plainId)}`;
  }

  get namespaceParam() {
    return `namespace=${encodeURIComponent(
      this.job.get('namespace.name') || 'default'
    )}`;
  }

  @task
  *load() {
    try {
      this.definition = yield this.job.fetchRawDefinition();
      const group = (this.definition.TaskGroups || []).find(
        (tg) => tg.Name === this.args.group.name
      );
      const scaling = group?.Scaling;
      this.original = scaling ? JSON.stringify(scaling, null, 2) : '';
      this.text = JSON.stringify(scaling || TEMPLATE, null, 2);
    } catch (e) {
      this.loadError = 'The job definition could not be loaded.';
    }
  }

  jobWith(text) {
    const job = JSON.parse(JSON.stringify(this.definition));
    const scaling = JSON.parse(text);
    job.TaskGroups = job.TaskGroups.map((tg) =>
      tg.Name === this.args.group.name ? { ...tg, Scaling: scaling } : tg
    );
    delete job.Stop;
    return job;
  }

  @action
  setText(text) {
    this.text = text;
  }

  @action
  async plan(text) {
    const job = this.jobWith(text);
    const response = await this.token.authorizedRequest(
      `${this.url}/plan?${this.namespaceParam}`,
      { method: 'POST', body: JSON.stringify({ Job: job, Diff: true }) }
    );
    return check(response);
  }

  @action
  async submit(text) {
    const job = this.jobWith(text);
    const response = await this.token.authorizedRequest(
      `${this.url}?${this.namespaceParam}`,
      { method: 'POST', body: JSON.stringify({ Job: job }) }
    );
    await check(response);
    this.notifications.add({
      title: 'Scaling policy saved',
      message: `A new version of ${this.job.name} was submitted.`,
      color: 'success',
    });
    this.args.onSaved?.();
  }
}
