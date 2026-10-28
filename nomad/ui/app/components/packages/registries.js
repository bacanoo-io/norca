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
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  listPackRegistries,
  upsertPackRegistry,
  syncPackRegistry,
  deletePackRegistry,
} from 'nomad-ui/utils/releases';

export const REGISTRY_TYPES = [
  {
    value: 'oci',
    label: 'OCI registry',
    placeholder: 'oci://ghcr.io/org/packs',
    help: 'An OCI repository of packs. Public registries only for now.',
  },
  {
    value: 'git',
    label: 'Git repository',
    placeholder: 'https://github.com/org/packs.git',
    help: 'A Git repository of packs; the reference is a branch or tag.',
  },
  {
    value: 'dir',
    label: 'Server directory',
    placeholder: '/srv/norca/packs',
    help: 'A directory on the servers, or builtin for the packs that ship with Norca.',
  },
  {
    value: 'bundle',
    label: 'Offline bundle',
    placeholder: '/srv/norca/bundles/packs.tar',
    help: 'A bundle made with norca pack bundle create, on the servers.',
  },
];

// Periodic syncs; the servers accept any duration of a minute or more.
export const SYNC_INTERVALS = [
  { value: '', label: 'On request only' },
  { value: '15m', label: 'Every 15 minutes' },
  { value: '1h', label: 'Every hour' },
  { value: '6h', label: 'Every 6 hours' },
  { value: '24h', label: 'Every day' },
];

// Go durations as the servers write them (1h0m0s, 15m) in milliseconds.
function durationMs(text) {
  const units = { h: 3600000, m: 60000, s: 1000 };
  let ms = 0;
  for (const [, n, u] of String(text || '').matchAll(
    /(\d+(?:\.\d+)?)([hms])/g
  )) {
    ms += Number(n) * units[u];
  }
  return ms;
}

const PHASE_TONES = { Ready: 'success', Syncing: 'info', Error: 'danger' };

/**
 * The cluster pack registries (packs.norca.io/Registry). The servers fetch
 * a registry when it is saved or synced and index its packs for the
 * catalog; trusted keys verify the signatures of the packs.
 */
export default class PackagesRegistries extends Component {
  @service token;
  @service system;
  @service notifications;

  @tracked registries = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked syncing = null;

  @tracked editing = null; // null, or { isNew }
  @tracked formName = '';
  @tracked formType = 'oci';
  @tracked formSource = '';
  @tracked formRef = '';
  @tracked formInterval = '';
  @tracked formKeys = '';
  @tracked isSaving = false;
  @tracked formError = null;

  @tracked toDelete = null;
  @tracked isDeleting = false;

  types = REGISTRY_TYPES;

  setup = modifier(() => {
    this.load();
  });

  get region() {
    return this.system.activeRegion || '';
  }

  get rows() {
    return this.registries.map((r) => {
      const status = r.Status || {};
      const phase = status.Phase || 'Pending';
      return {
        registry: r,
        name: r.Name,
        type:
          REGISTRY_TYPES.find((t) => t.value === r.Spec?.Type)?.label ||
          r.Spec?.Type,
        source: r.Spec?.Source,
        ref: r.Spec?.DefaultRef,
        keys: (r.Spec?.TrustedKeys || []).length,
        phase,
        phaseTone: PHASE_TONES[phase] || 'neutral',
        message: status.Message,
        packs: status.PackCount || 0,
        lastSync: status.LastSyncedAt
          ? moment(status.LastSyncedAt).fromNow()
          : 'Never',
        lastSyncTitle: status.LastSyncedAt || '',
        // After a failed sync the packs are those of the last good one.
        lastGoodSync:
          phase === 'Error' && status.LastGoodSyncAt
            ? moment(status.LastGoodSyncAt).fromNow()
            : null,
        nextSync: this.nextSync(r.Spec?.SyncInterval, status.LastSyncedAt),
      };
    });
  }

  nextSync(interval, last) {
    const ms = durationMs(interval);
    if (!ms) return null;
    const next = last ? moment(last).add(ms, 'ms') : moment();
    return next.isAfter(moment()) ? next.fromNow() : 'due';
  }

  get intervalOptions() {
    if (SYNC_INTERVALS.some((o) => o.value === this.formInterval)) {
      return SYNC_INTERVALS;
    }
    return [
      ...SYNC_INTERVALS,
      { value: this.formInterval, label: `Every ${this.formInterval}` },
    ];
  }

  get typeInfo() {
    return (
      REGISTRY_TYPES.find((t) => t.value === this.formType) || REGISTRY_TYPES[0]
    );
  }

  async load() {
    this.isLoading = true;
    this.loadError = null;
    try {
      const r = await waitForPromise(
        listPackRegistries(this.token.token, this.region)
      );
      this.registries = r.body || [];
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action openAdd() {
    this.fill({
      Name: '',
      Spec: { Type: 'oci', Source: '', DefaultRef: '', TrustedKeys: [] },
    });
    this.editing = { isNew: true };
  }

  @action openEdit(registry) {
    this.fill(registry);
    this.editing = { isNew: false };
  }

  @action presetBuiltin() {
    if (this.editing?.isNew && !this.formName) this.formName = 'builtin';
    this.formType = 'dir';
    this.formSource = 'builtin';
  }

  fill(registry) {
    const spec = registry.Spec || {};
    this.formName = registry.Name || '';
    this.formType = spec.Type || 'oci';
    this.formSource = spec.Source || '';
    this.formRef = spec.DefaultRef || '';
    this.formInterval = spec.SyncInterval || '';
    this.formKeys = (spec.TrustedKeys || []).join('\n\n');
    this.formError = null;
  }

  @action closeEdit() {
    this.editing = null;
  }

  @action setName(v) {
    this.formName = v;
  }

  @action setType(v) {
    this.formType = v;
  }

  @action setSource(v) {
    this.formSource = v;
  }

  @action setRef(v) {
    this.formRef = v;
  }

  @action setInterval(v) {
    this.formInterval = v;
  }

  @action setKeys(v) {
    this.formKeys = v;
  }

  // Keys are PEM blocks or single lines (ssh, cosign key ids), separated by
  // blank lines.
  get trustedKeys() {
    return this.formKeys
      .split(/\n\s*\n/)
      .map((k) => k.trim())
      .filter(Boolean);
  }

  @action async save() {
    const name = this.formName.trim();
    if (!name) {
      this.formError = 'The name is required';
      return;
    }
    if (!this.formSource.trim()) {
      this.formError = 'The source is required';
      return;
    }
    this.isSaving = true;
    this.formError = null;
    try {
      // Saving fetches and indexes the registry before it answers.
      const r = await upsertPackRegistry(this.token.token, this.region, name, {
        type: this.formType,
        source: this.formSource.trim(),
        defaultRef: this.formRef.trim() || undefined,
        syncInterval: this.formInterval || undefined,
        trustedKeys: this.trustedKeys,
      });
      const status = r.body?.Status || {};
      this.notifications.add(
        status.Phase === 'Error'
          ? {
              title: `${name} saved, but the sync failed`,
              message: status.Message,
              color: 'critical',
            }
          : {
              title: `${name} saved with ${status.PackCount || 0} packs`,
              color: 'success',
            }
      );
      this.editing = null;
      await this.load();
    } catch (e) {
      this.formError = errorMessage(e);
    } finally {
      this.isSaving = false;
    }
  }

  @action async sync(row) {
    this.syncing = row.name;
    try {
      const r = await syncPackRegistry(this.token.token, this.region, row.name);
      const status = r.body?.Status || {};
      this.notifications.add(
        status.Phase === 'Error'
          ? {
              title: `${row.name} could not be synced`,
              message: status.Message,
              color: 'critical',
            }
          : {
              title: `${row.name} synced: ${status.PackCount || 0} packs`,
              color: 'success',
            }
      );
      await this.load();
    } catch (e) {
      this.notifications.add({
        title: `${row.name} could not be synced`,
        message: errorMessage(e),
        color: 'critical',
      });
    } finally {
      this.syncing = null;
    }
  }

  @action askDelete(row) {
    this.toDelete = row;
  }

  @action cancelDelete() {
    this.toDelete = null;
  }

  @action async confirmDelete() {
    const name = this.toDelete?.name;
    if (!name) return;
    this.isDeleting = true;
    try {
      await deletePackRegistry(this.token.token, this.region, name);
      this.notifications.add({ title: `${name} removed`, color: 'success' });
      this.toDelete = null;
      await this.load();
    } catch (e) {
      this.notifications.add({
        title: `${name} could not be removed`,
        message: errorMessage(e),
        color: 'critical',
      });
    } finally {
      this.isDeleting = false;
    }
  }
}
