/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';

const PAGE = 8;

async function errorText(response) {
  const text = await response.text().catch(() => '');
  return text.trim() || `${response.status} ${response.statusText}`;
}

// Stable JSON with sorted keys, so that two versions compare by content.
function canonical(value) {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === 'object') {
    return Object.keys(value)
      .sort()
      .reduce((out, key) => {
        out[key] = canonical(value[key]);
        return out;
      }, {});
  }
  return value;
}

// Fields that change on every registration and only add noise to a diff.
const VOLATILE = [
  'CreateIndex',
  'ModifyIndex',
  'JobModifyIndex',
  'SubmitTime',
  'Status',
  'StatusDescription',
  'Stable',
  'Version',
];

export function jobText(job) {
  const copy = { ...(job || {}) };
  VOLATILE.forEach((key) => delete copy[key]);
  return JSON.stringify(canonical(copy), null, 2);
}

/**
 * Versions of a job retrievable from the backups (UI plan, job details:
 * Versions). Nomad keeps only the latest versions of a job; a backup holds
 * the versions that existed when it was taken. A version is compared with
 * the current definition and restored as a new version of the job through
 * a selective restore (previewed with a dry run first), so it goes through
 * the admission chain and the quota like any submission.
 *
 * Reading the content of an encrypted backup needs the age identity; it is
 * sent with the request only and never stored.
 */
export default class JobsBackupVersions extends Component {
  @service token;
  @service notifications;
  @service router;

  @tracked isOpen = false;
  @tracked backups = null;
  @tracked listError = null;
  @tracked shown = PAGE;
  @tracked selected = null;
  @tracked versions = null;
  @tracked contentsError = null;
  @tracked identity = '';
  @tracked compared = null;
  @tracked currentText = '';
  @tracked preview = null;
  @tracked previewVersion = null;
  @tracked restoreError = null;
  @tracked isPreviewing = false;

  get job() {
    return this.args.job;
  }

  get namespace() {
    return this.job.get('namespace.name') || 'default';
  }

  get visibleBackups() {
    return (this.backups || []).slice(0, this.shown);
  }

  get hasMore() {
    return (this.backups || []).length > this.shown;
  }

  get canRestore() {
    return (
      !this.restoreError &&
      !!this.preview?.length &&
      !this.preview.some((r) => r.Error)
    );
  }

  @action
  open() {
    this.isOpen = true;
    this.loadBackups.perform();
  }

  @action
  showMore() {
    this.shown += PAGE;
  }

  @action
  setIdentity(value) {
    this.identity = value;
  }

  @task({ restartable: true })
  *loadBackups() {
    this.listError = null;
    try {
      const response = yield this.token.authorizedRequest(
        '/v1/operator/backups'
      );
      if (response.status === 403) {
        this.listError =
          'Your token cannot read the backups. Reading backups needs operator read access.';
        return;
      }
      if (!response.ok) {
        this.listError = yield errorText(response);
        return;
      }
      const body = yield response.json();
      this.backups = (body?.Backups || []).filter((b) => b.Status !== 'failed');
    } catch (e) {
      this.listError = 'The backups could not be loaded.';
    }
  }

  @task({ restartable: true })
  *loadVersions(backup) {
    this.selected = backup;
    this.versions = null;
    this.contentsError = null;
    this.compared = null;
    const url = `/v1/operator/backup/${encodeURIComponent(
      backup.ID
    )}/contents?type=job-versions&job=${encodeURIComponent(
      this.job.plainId
    )}&namespace=${encodeURIComponent(this.namespace)}`;
    try {
      const response = yield this.token.authorizedRequest(url, {
        method: 'POST',
        body: JSON.stringify({ AgeIdentity: this.identity || '' }),
      });
      if (!response.ok) {
        this.contentsError = yield errorText(response);
        return;
      }
      const contents = yield response.json();
      this.versions = (contents?.JobVersions || [])
        .filter((v) => (v.Namespace || 'default') === this.namespace)
        .sort((a, b) => (b.Version || 0) - (a.Version || 0));
    } catch (e) {
      this.contentsError = 'The content of the backup could not be read.';
    }
  }

  @task({ drop: true })
  *compare(version) {
    const definition = yield this.job.fetchRawDefinition();
    this.currentText = jobText(definition);
    this.compared = { version, text: jobText(version) };
  }

  @action
  closeCompare() {
    this.compared = null;
  }

  restoreBody(version, dryRun) {
    return JSON.stringify({
      Mode: 'selective',
      DryRun: dryRun,
      AgeIdentity: this.identity || '',
      Items: [
        {
          Type: 'job',
          Namespace: this.namespace,
          ID: this.job.plainId,
          Version: version.Version,
        },
      ],
    });
  }

  @task({ drop: true })
  *previewRestore(version) {
    this.restoreError = null;
    this.preview = null;
    this.previewVersion = version;
    this.isPreviewing = true;
    try {
      const response = yield this.token.authorizedRequest(
        `/v1/operator/backup/${encodeURIComponent(this.selected.ID)}/restore`,
        { method: 'POST', body: this.restoreBody(version, true) }
      );
      if (!response.ok) {
        this.restoreError = yield errorText(response);
        this.preview = [];
        return;
      }
      const body = yield response.json();
      this.preview = body?.Results || [];
    } catch (e) {
      this.restoreError = 'The restore could not be previewed.';
      this.preview = [];
    }
  }

  @action
  closePreview() {
    this.isPreviewing = false;
    this.preview = null;
    this.previewVersion = null;
    this.restoreError = null;
  }

  @task({ drop: true })
  *restore() {
    const version = this.previewVersion;
    this.restoreError = null;
    try {
      const response = yield this.token.authorizedRequest(
        `/v1/operator/backup/${encodeURIComponent(this.selected.ID)}/restore`,
        { method: 'POST', body: this.restoreBody(version, false) }
      );
      if (!response.ok) {
        this.restoreError = yield errorText(response);
        return;
      }
      const body = yield response.json();
      const failed = (body?.Results || []).find((r) => r.Error);
      if (failed) {
        this.restoreError = failed.Error;
        return;
      }
      this.notifications.add({
        title: 'Version restored',
        message: `Version ${version.Version} of ${this.job.name} from backup ${this.selected.ID} was submitted as a new version.`,
        color: 'success',
      });
      this.closePreview();
      yield this.job.reload();
      this.router.transitionTo('jobs.job.index', this.job.idWithNamespace);
    } catch (e) {
      this.restoreError = 'The version could not be restored.';
    }
  }
}
