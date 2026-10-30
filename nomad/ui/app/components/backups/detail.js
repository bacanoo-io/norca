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
  getBackup,
  listRestores,
  updateBackup,
  deleteBackup,
  downloadURL,
  backupSize,
  formatSize,
  isSet,
  statusTone,
  TRIGGER_LABELS,
  CONTENT_TYPES,
} from 'nomad-ui/utils/backups';

const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'contents', label: 'Contents' },
  { id: 'compare', label: 'Compare' },
  { id: 'restores', label: 'Restores' },
];

/**
 * A backup: its manifest (time, trigger, Raft index, destinations, content
 * counts, labels), browsing its content, comparing it and the restores made
 * from it. Pin, download link (S3 destinations only) and delete for tokens
 * with backup write.
 */
export default class BackupsDetail extends Component {
  @service token;
  @service system;
  @service router;
  @service eventStream;
  @service abilities;
  @service notifications;

  @tracked backup = null;
  @tracked restores = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked notFound = false;
  @tracked isPinning = false;
  @tracked actionError = null;
  @tracked confirmingDelete = false;
  @tracked isDeleting = false;
  @tracked deleteError = null;
  @tracked download = null;
  @tracked isLinking = false;

  get region() {
    return this.system.activeRegion || '';
  }

  get canWrite() {
    return this.abilities.can('write backup');
  }

  get tabs() {
    return TABS.map((t) =>
      t.id === 'restores' ? { ...t, count: this.restores.length } : t
    );
  }

  get tab() {
    return TABS.some((t) => t.id === this.args.tab)
      ? this.args.tab
      : 'overview';
  }

  get isUsable() {
    return this.backup && this.backup.Status !== 'failed';
  }

  // Only S3 destinations sign download URLs; a backup kept only on
  // directory destinations (file:// locations, shown in the destinations
  // table) has no download link.
  get canDownload() {
    const destinations = this.backup?.Destinations || [];
    if (!destinations.length) return true;
    return destinations.some(
      (d) =>
        d.Status === 'verified' &&
        !String(d.Location || '').startsWith('file://')
    );
  }

  get details() {
    const b = this.backup;
    if (!b) return [];
    return [
      { title: 'Region', value: b.Region || this.region || 'local' },
      { title: 'Trigger', value: TRIGGER_LABELS[b.Trigger] || b.Trigger },
      {
        title: 'Started',
        value: isSet(b.StartedAt)
          ? new Date(b.StartedAt).toLocaleString()
          : '–',
      },
      {
        title: 'Completed',
        value: isSet(b.CompletedAt)
          ? new Date(b.CompletedAt).toLocaleString()
          : '–',
      },
      { title: 'Raft index', value: b.RaftIndex, mono: true },
      { title: 'Size', value: formatSize(backupSize(b)) },
      { title: 'Version', value: `Norca ${b.NorcaVersion || '?'}` },
      { title: 'Authoritative region', value: b.AuthoritativeRegion || '–' },
      { title: 'Path', value: b.Path || '–', mono: true },
    ];
  }

  get summary() {
    const s = this.backup?.Summary || {};
    const keys = {
      jobs: 'jobs',
      deployments: 'deployments',
      allocations: 'allocations',
      variables: 'variables',
      namespaces: 'namespaces',
      'acl-policies': 'acl_policies',
      'acl-roles': 'acl_roles',
    };
    return CONTENT_TYPES.map((t) => ({
      ...t,
      count: s[keys[t.id]] ?? 0,
    }));
  }

  get destinations() {
    return (this.backup?.Destinations || []).map((d) => ({
      ...d,
      tone: statusTone(d.Status),
      size: formatSize(
        Object.values(d.Objects || {}).reduce(
          (sum, o) => sum + (o.Size || 0),
          0
        )
      ),
      duration: d.Duration ? `${Math.round(d.Duration / 1e9)} s` : '–',
    }));
  }

  get labels() {
    return Object.entries(this.backup?.Labels || {}).map(([k, v]) => ({
      key: k,
      value: v,
    }));
  }

  get restoreRows() {
    return this.restores.map((r) => ({
      ...r,
      shortId: String(r.ID).split('-')[0],
      tone: statusTone(r.Status),
    }));
  }

  async load() {
    try {
      const { body, index } = await getBackup(
        this.token,
        this.region,
        this.args.backupId
      );
      this.backup = body;
      this.loadError = null;
      this.notFound = false;
      try {
        const restores = await listRestores(this.token, this.region);
        this.restores = (restores.body || []).filter(
          (r) => r.BackupID === this.args.backupId
        );
      } catch (e) {
        this.restores = [];
      }
      return index;
    } catch (e) {
      if (e.status === 404) this.notFound = true;
      else this.loadError = errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  live = modifier((element, [backupId, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['Backup'],
      region,
      onEvents: (events) => {
        const relevant = events.some(
          (e) => e.Key === backupId || e.Payload?.Restore?.BackupID === backupId
        );
        if (relevant) waitForPromise(this.load());
      },
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  @action
  selectTab(id) {
    this.args.onTabChange?.(id);
  }

  @action
  async togglePin() {
    this.isPinning = true;
    this.actionError = null;
    try {
      const { body } = await updateBackup(
        this.token,
        this.region,
        this.backup.ID,
        { Pinned: !this.backup.Pinned }
      );
      this.backup = body;
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isPinning = false;
    }
  }

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
  async deleteBackup() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deleteBackup(this.token, this.region, this.backup.ID);
      this.notifications.add({
        title: `Backup ${this.backup.ID} deleted`,
        color: 'success',
      });
      this.confirmingDelete = false;
      this.router.transitionTo('backups.index');
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }

  @action
  async createDownloadLink() {
    this.isLinking = true;
    this.actionError = null;
    try {
      const { body } = await downloadURL(
        this.token,
        this.region,
        this.backup.ID
      );
      this.download = body;
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isLinking = false;
    }
  }

  @action
  closeDownload() {
    this.download = null;
  }

  @action
  dismissError() {
    this.actionError = null;
  }
}
