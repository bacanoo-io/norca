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
import { listBackups, backupContents } from 'nomad-ui/utils/backups';

const DEPTHS = [
  { value: '3', label: 'the 3 latest backups' },
  { value: '10', label: 'the 10 latest backups' },
  { value: '30', label: 'the 30 latest backups' },
  { value: 'all', label: 'every backup' },
];

/**
 * The history of a job across the backups of the region: the versions kept
 * in the backups (Nomad itself keeps only the last few), its deployments and
 * its allocations. Each backup is decrypted on the server, so the search
 * covers a chosen number of the latest backups. A version can be restored as
 * a new version of the job, or compared with the current job.
 */
export default class BackupsJobHistory extends Component {
  @service token;
  @service system;
  @service backups;
  @service router;

  @tracked jobInput = this.args.job || '';
  @tracked namespaceInput = this.args.namespace || 'default';
  @tracked depth = '3';
  @tracked tab = 'versions';
  @tracked isLoading = false;
  @tracked error = null;
  @tracked identityError = null;
  @tracked searched = 0;
  @tracked versions = [];
  @tracked deployments = [];
  @tracked allocations = [];
  @tracked currentVersions = [];

  depths = DEPTHS;

  get region() {
    return this.system.activeRegion || '';
  }

  get tabs() {
    return [
      { id: 'versions', label: 'Versions', count: this.versions.length },
      {
        id: 'deployments',
        label: 'Deployments',
        count: this.deployments.length,
      },
      {
        id: 'allocations',
        label: 'Allocations',
        count: this.allocations.length,
      },
    ];
  }

  get versionRows() {
    const current = new Set(this.currentVersions);
    return this.versions.map((v) => ({
      ...v,
      keptNow: current.has(v.version),
      submitted: v.submitTime
        ? new Date(v.submitTime / 1e6).toLocaleString()
        : '–',
    }));
  }

  async loadCurrentVersions(job, namespace) {
    try {
      const { body } = await apiRequest(
        this.token,
        `/v1/job/${encodeURIComponent(job)}/versions`,
        { params: { region: this.region, namespace } }
      );
      this.currentVersions = (body?.Versions || []).map((v) => v.Version);
    } catch (e) {
      this.currentVersions = [];
    }
  }

  async search() {
    const job = this.args.job;
    const namespace = this.args.namespace || 'default';
    if (!job || !this.backups.hasIdentity) return;
    this.isLoading = true;
    this.error = null;
    try {
      const { body } = await listBackups(this.token, this.region);
      let list = (body?.Backups || []).filter((b) => b.Status !== 'failed');
      if (this.depth !== 'all') list = list.slice(0, Number(this.depth));
      const versions = new Map();
      const deployments = new Map();
      const allocations = new Map();
      for (const backup of list) {
        const base = {
          region: this.region,
          id: backup.ID,
          identity: this.backups.identity,
          job,
          namespace,
        };
        const [v, d, a] = await Promise.all([
          backupContents(this.token, { ...base, type: 'job-versions' }),
          backupContents(this.token, { ...base, type: 'deployments' }),
          backupContents(this.token, { ...base, type: 'allocations' }),
        ]);
        for (const j of v.body?.JobVersions || []) {
          if (!versions.has(j.Version))
            versions.set(j.Version, {
              version: j.Version,
              submitTime: j.SubmitTime,
              status: j.Stop ? 'stopped' : j.Status,
              backupId: backup.ID,
              backups: [],
            });
          versions.get(j.Version).backups.push(backup.ID);
        }
        for (const dep of d.body?.Deployments || [])
          if (!deployments.has(dep.ID))
            deployments.set(dep.ID, { ...dep, backupId: backup.ID });
        for (const alloc of a.body?.Allocations || [])
          if (!allocations.has(alloc.ID))
            allocations.set(alloc.ID, { ...alloc, backupId: backup.ID });
      }
      this.versions = [...versions.values()].sort(
        (x, y) => y.version - x.version
      );
      this.deployments = [...deployments.values()]
        .sort((x, y) => (y.ModifyIndex || 0) - (x.ModifyIndex || 0))
        .map((dep) => ({ ...dep, shortId: dep.ID.split('-')[0] }));
      this.allocations = [...allocations.values()]
        .sort((x, y) => (y.ModifyIndex || 0) - (x.ModifyIndex || 0))
        .map((al) => ({ ...al, shortId: al.ID.split('-')[0] }));
      this.searched = list.length;
      await this.loadCurrentVersions(job, namespace);
    } catch (e) {
      const message = errorMessage(e);
      if (/identity|recipient|decrypt/i.test(message)) {
        this.backups.forgetIdentity();
        this.identityError = message;
      } else this.error = message;
    } finally {
      this.isLoading = false;
    }
  }

  loader = modifier((element, [job, , hasIdentity]) => {
    if (job && hasIdentity)
      waitForPromise(Promise.resolve().then(() => this.search()));
  });

  @action setJob(value) {
    this.jobInput = value;
  }

  @action setNamespace(value) {
    this.namespaceInput = value;
  }

  @action setDepth(value) {
    this.depth = value;
  }

  @action selectTab(id) {
    this.tab = id;
  }

  @action
  submit(event) {
    event.preventDefault();
    const job = this.jobInput.trim();
    if (!job) return;
    const params = { job, namespace: this.namespaceInput.trim() || 'default' };
    if (
      params.job === this.args.job &&
      params.namespace === this.args.namespace
    )
      waitForPromise(this.search());
    else this.args.onSearch?.(params);
  }

  @action
  restoreVersion(row) {
    this.backups.setDraft(row.backupId, [
      {
        Type: 'job',
        Namespace: this.args.namespace || 'default',
        ID: this.args.job,
        Version: row.version,
      },
    ]);
    this.router.transitionTo('backups.restore', row.backupId, {
      queryParams: { mode: 'selective' },
    });
  }
}
