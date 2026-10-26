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
  listBackups,
  listRestores,
  updateBackup,
  backupSize,
  formatSize,
  isSet,
  scheduleLabel,
  statusTone,
  TRIGGER_LABELS,
} from 'nomad-ui/utils/backups';
import LocalList from 'nomad-ui/utils/local-list';

const POLL_MS = 30000;

/**
 * The backups page: per region in scope, the state of the schedule, the
 * restore requests that wait for approval and the backups, newest first.
 * Updates from the Backup topic of the event stream.
 */
export default class BackupsList extends Component {
  @service token;
  @service frame;
  @service system;
  @service eventStream;
  @service abilities;
  @service notifications;

  @tracked regions = [];
  @tracked isLoading = true;
  @tracked backupNowOpen = false;
  @tracked pinning = null;
  @tracked actionError = null;

  get scopedRegions() {
    return this.frame.scopedRegions;
  }

  get isAllRegions() {
    return this.scopedRegions.length > 1;
  }

  get canWrite() {
    return this.abilities.can('write backup');
  }

  get scopeLabel() {
    return this.isAllRegions
      ? 'All regions'
      : this.scopedRegions[0] || 'Local region';
  }

  get columns() {
    const columns = [
      { key: 'ID', label: 'Backup', sortable: true, hideable: false },
      { key: 'time', label: 'Completed', sortable: true },
    ];
    if (this.isAllRegions)
      columns.push({ key: 'region', label: 'Region', sortable: true });
    columns.push(
      { key: 'triggerLabel', label: 'Trigger', sortable: true },
      { key: 'Status', label: 'Status', sortable: true },
      { key: 'size', label: 'Size', sortable: true },
      { key: 'destinations', label: 'Destinations' },
      { key: 'labels', label: 'Labels' },
      { key: 'pinned', label: 'Pinned', sortable: true }
    );
    return columns;
  }

  // Newest first until the user sorts by another column.
  list = new LocalList({
    sortBy: 'time',
    sortDescending: true,
    searchProps: ['ID', 'labelText', 'triggerLabel'],
  });

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get schedules() {
    return this.regions.map((r) => {
      const s = r.schedule || {};
      return {
        region: r.region,
        label: r.region || 'Local region',
        forbidden: r.forbidden,
        error: r.error,
        schedule: scheduleLabel(s),
        enabled: !!s.Enabled,
        destinations: (s.Destinations || []).join(', '),
        lastSuccess: isSet(s.LastSuccess) ? s.LastSuccess : null,
        lastSuccessID: s.LastSuccessID,
        lastError: s.LastError,
        nextDue: isSet(s.NextDue) ? s.NextDue : null,
        overdue: !!s.Overdue,
      };
    });
  }

  get warnings() {
    return this.schedules.filter((s) => s.overdue || s.lastError);
  }

  get pendingRestores() {
    return this.regions.flatMap((r) =>
      (r.restores || [])
        .filter((x) => x.Status === 'pending')
        .map((x) => ({ ...x, region: r.region }))
    );
  }

  get recentRestores() {
    return this.regions
      .flatMap((r) =>
        (r.restores || []).map((x) => ({ ...x, region: r.region }))
      )
      .sort((a, b) => (a.CreateTime < b.CreateTime ? 1 : -1))
      .slice(0, 5)
      .map((x) => ({ ...x, tone: statusTone(x.Status) }));
  }

  get rows() {
    return this.regions.flatMap((r) =>
      (r.backups || []).map((b) => ({
        ...b,
        region: r.region,
        size: backupSize(b),
        sizeLabel: formatSize(backupSize(b)),
        triggerLabel: TRIGGER_LABELS[b.Trigger] || b.Trigger,
        tone: statusTone(b.Status),
        time: isSet(b.CompletedAt) ? b.CompletedAt : b.StartedAt,
        labelList: Object.entries(b.Labels || {}).map(([k, v]) => `${k}=${v}`),
        labelText: Object.entries(b.Labels || {})
          .map(([k, v]) => `${k}=${v}`)
          .join(' '),
        pinned: b.Pinned ? 1 : 0,
        destinationList: (b.Destinations || []).map((d) => ({
          name: d.Name,
          status: d.Status,
          tone: statusTone(d.Status),
          error: d.Error,
        })),
      }))
    );
  }

  get hasBackups() {
    return this.regions.some((r) => (r.backups || []).length);
  }

  get emptyMessage() {
    const s = this.schedules[0];
    if (s?.enabled && s.nextDue) {
      return `No backup yet. The first scheduled backup runs ${new Date(
        s.nextDue
      ).toLocaleString()}, or start one manually.`;
    }
    return 'No backup yet. Configure a schedule in the backup block of the server configuration, or start one manually.';
  }

  async loadRegion(region) {
    const result = { region, backups: [], schedule: null, restores: [] };
    try {
      const { body, index } = await listBackups(this.token, region);
      result.backups = body?.Backups || [];
      result.schedule = body?.Schedule || null;
      result.index = index;
    } catch (e) {
      if (e.status === 403) result.forbidden = true;
      else result.error = errorMessage(e);
      return result;
    }
    try {
      const { body } = await listRestores(this.token, region);
      result.restores = body || [];
    } catch (e) {
      result.restores = [];
    }
    return result;
  }

  @action
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

  live = modifier((element, [regions]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: ['Backup'],
        region,
        onEvents: () => waitForPromise(this.refreshRegion(region)),
        onResync: async () => (await this.refreshRegion(region))?.index || 0,
      })
    );
    let timer = null;
    if (config.environment !== 'test') {
      timer = setInterval(() => {
        const open = regions.every(
          (r) => this.eventStream.stateFor(r, '*') === 'open'
        );
        if (!open) waitForPromise(this.load());
      }, POLL_MS);
    }
    return () => {
      unsubscribers.forEach((u) => u());
      clearInterval(timer);
    };
  });

  @action
  openBackupNow() {
    this.backupNowOpen = true;
  }

  @action
  closeBackupNow() {
    this.backupNowOpen = false;
  }

  @action
  async backupStarted(region) {
    this.backupNowOpen = false;
    await this.refreshRegion(region);
  }

  @action
  async togglePin(row) {
    this.pinning = row.ID;
    this.actionError = null;
    try {
      await updateBackup(this.token, row.region, row.ID, {
        Pinned: !row.Pinned,
      });
      await this.refreshRegion(row.region);
    } catch (e) {
      this.actionError = `Could not ${row.Pinned ? 'unpin' : 'pin'} ${
        row.ID
      }: ${errorMessage(e)}`;
    } finally {
      this.pinning = null;
    }
  }

  @action
  dismissError() {
    this.actionError = null;
  }
}
