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
import {
  backupDiff,
  listBackups,
  flattenJobDiff,
  CONTENT_TYPES,
} from 'nomad-ui/utils/backups';

const CHANGE = {
  added: { color: 'green', label: 'added' },
  deleted: { color: 'red', label: 'deleted' },
  modified: { color: 'orange', label: 'modified' },
};

/**
 * Comparing a backup with the current state or with another backup of the
 * region: new, deleted and modified objects per type, and the field
 * differences of a job (the server's job diff).
 */
export default class BackupsCompare extends Component {
  @service token;
  @service frame;
  @service backups;

  @tracked others = [];
  @tracked objects = [];
  @tracked isLoading = false;
  @tracked error = null;
  @tracked identityError = null;
  @tracked jobDiff = null;
  @tracked jobDiffFor = null;
  @tracked jobDiffLoading = false;

  get against() {
    return this.args.against || 'current';
  }

  get againstLabel() {
    return this.against === 'current' ? 'the current state' : this.against;
  }

  get type() {
    return CONTENT_TYPES.some((t) => t.id === this.args.type)
      ? this.args.type
      : '';
  }

  get againstOptions() {
    return [
      { value: 'current', label: 'Current state' },
      ...this.others
        .filter((b) => b.ID !== this.args.backup.ID && b.Status !== 'failed')
        .map((b) => ({ value: b.ID, label: `Backup ${b.ID}` })),
    ];
  }

  get typeOptions() {
    return [
      { value: '', label: 'All types' },
      ...CONTENT_TYPES.map((t) => ({ value: t.id, label: t.label })),
    ];
  }

  get counts() {
    const c = { added: 0, deleted: 0, modified: 0 };
    this.objects.forEach((o) => (c[o.Change] = (c[o.Change] || 0) + 1));
    return c;
  }

  get rows() {
    return this.objects.map((o) => ({
      ...o,
      change: CHANGE[o.Change] || { color: 'secondary', label: o.Change },
      typeLabel: CONTENT_TYPES.find((t) => t.id === o.Type)?.label || o.Type,
      from: this.side(o.FromVersion, o.FromStatus),
      to: this.side(o.ToVersion, o.ToStatus),
      canDiff: o.Type === 'jobs' && o.Change === 'modified',
    }));
  }

  get jobDiffRows() {
    return flattenJobDiff(this.jobDiff);
  }

  side(version, status) {
    const parts = [];
    if (version !== null && version !== undefined) parts.push(`v${version}`);
    if (status) parts.push(status);
    return parts.join(', ') || '–';
  }

  async loadOthers() {
    try {
      const { body } = await listBackups(this.token, this.args.region);
      this.others = body?.Backups || [];
    } catch (e) {
      this.others = [];
    }
  }

  async load() {
    if (!this.backups.hasIdentity) return;
    this.isLoading = true;
    this.error = null;
    this.jobDiff = null;
    this.jobDiffFor = null;
    try {
      const params = {
        against: this.against,
        namespace: this.frame.namespace || '*',
      };
      if (this.type) params.type = this.type;
      const { body } = await backupDiff(this.token, {
        region: this.args.region,
        id: this.args.backup.ID,
        identity: this.backups.identity,
        ...params,
      });
      this.objects = body?.Objects || [];
      this.identityError = null;
    } catch (e) {
      const message = errorMessage(e);
      if (/identity|recipient|decrypt/i.test(message)) {
        this.backups.forgetIdentity();
        this.identityError = message;
      } else {
        this.error = message;
      }
      this.objects = [];
    } finally {
      this.isLoading = false;
    }
  }

  loader = modifier((element, [, , , hasIdentity]) => {
    waitForPromise(this.loadOthers());
    if (hasIdentity) waitForPromise(Promise.resolve().then(() => this.load()));
  });

  @action
  setAgainst(value) {
    this.args.onParamsChange?.({ against: value === 'current' ? null : value });
  }

  @action
  setType(value) {
    this.args.onParamsChange?.({ type: value || null, job: null });
  }

  @action
  async showJobDiff(row) {
    this.jobDiffFor = row;
    this.jobDiffLoading = true;
    try {
      const { body } = await backupDiff(this.token, {
        region: this.args.region,
        id: this.args.backup.ID,
        identity: this.backups.identity,
        against: this.against,
        type: 'jobs',
        job: row.ID,
        namespace: row.Namespace || 'default',
      });
      this.jobDiff = body?.JobDiff || null;
    } catch (e) {
      this.jobDiff = null;
      this.error = errorMessage(e);
    } finally {
      this.jobDiffLoading = false;
    }
  }

  @action
  closeJobDiff() {
    this.jobDiffFor = null;
    this.jobDiff = null;
  }
}
