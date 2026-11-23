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
  OLD_INDEX_DAYS,
  applyRiggerRegistry,
  deleteRiggerRegistry,
  indexAgeDays,
  listRiggerRegistries,
  syncRiggerRegistry,
} from 'nomad-ui/utils/rigger';

const NAME = /^[a-z0-9][a-z0-9-]{0,62}$/;

/**
 * The registries of Rigger (rigger.norca.io/Registry objects). A sync runs
 * only when asked for: the list shows when the last good one was, of which
 * commit, and an index older than OLD_INDEX_DAYS days.
 */
export default class PackagesRiggerRegistries extends Component {
  @service token;
  @service system;
  @service notifications;

  @tracked registries = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked syncing = null;

  @tracked editing = null;
  @tracked formName = '';
  @tracked formType = 'git';
  @tracked formSource = '';
  @tracked formDisplay = 'catalog';
  @tracked formAuthNamespace = '';
  @tracked formAuthPath = '';
  @tracked formKeys = '';
  @tracked formError = null;
  @tracked isSaving = false;

  @tracked toDelete = null;
  @tracked isDeleting = false;

  setup = modifier(() => {
    this.load();
  });

  get region() {
    return this.system.activeRegion || '';
  }

  get rows() {
    return this.registries.map((r) => {
      const s = r.status || {};
      const age = indexAgeDays(r);
      return {
        name: r.name,
        registry: r,
        type: r.spec.type,
        source: r.spec.source,
        display: r.spec.display || 'catalog',
        selected: (r.spec.selected || []).length,
        keys: (r.spec.trustedKeys || []).length,
        auth: r.spec.authSecret
          ? `${r.spec.authSecret.namespace || 'default'}/${
              r.spec.authSecret.path
            }`
          : '',
        phase: s.phase || 'Never synced',
        phaseTone:
          s.phase === 'Ready'
            ? 'success'
            : s.phase === 'Error'
            ? 'danger'
            : 'neutral',
        message: s.message,
        content: s.phase
          ? `${s.modules || 0} modules, ${s.stacks || 0} stacks`
          : '',
        lastGoodSync: s.lastGoodSyncAt
          ? moment(s.lastGoodSyncAt).fromNow()
          : '',
        lastGoodSyncTitle: s.lastGoodSyncAt || '',
        commit: (s.commit || '').slice(0, 12),
        isOld: age !== null && age > OLD_INDEX_DAYS,
        missing: (s.missing || []).map((m) =>
          m.namespace ? `${m.namespace}/${m.name}` : m.name
        ),
      };
    });
  }

  get types() {
    return [
      { value: 'git', label: 'Git repository' },
      { value: 'global', label: 'The global registry (only "global")' },
    ];
  }

  get displays() {
    return [
      { value: 'catalog', label: 'Every module and stack' },
      { value: 'selected', label: 'Only those selected' },
    ];
  }

  async load() {
    this.isLoading = true;
    this.loadError = null;
    try {
      const r = await waitForPromise(
        listRiggerRegistries(this.token, this.region)
      );
      this.registries = r.body.sort((a, b) => a.name.localeCompare(b.name));
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action async sync(row) {
    this.syncing = row.name;
    try {
      await waitForPromise(
        syncRiggerRegistry(this.token, this.region, row.name)
      );
      this.notifications.add({ title: `${row.name} synced`, color: 'success' });
    } catch (e) {
      this.notifications.add({
        title: `${row.name} could not be synced`,
        message: errorMessage(e),
        color: 'critical',
      });
    } finally {
      this.syncing = null;
      await this.load();
    }
  }

  @action openAdd() {
    this.editing = { isNew: true };
    this.formName = '';
    this.formType = 'git';
    this.formSource = '';
    this.formDisplay = 'catalog';
    this.formAuthNamespace = '';
    this.formAuthPath = '';
    this.formKeys = '';
    this.formError = null;
  }

  @action openEdit(registry) {
    const spec = registry.spec || {};
    this.editing = { isNew: false, registry };
    this.formName = registry.name;
    this.formType = spec.type || 'git';
    this.formSource = spec.source || '';
    this.formDisplay = spec.display || 'catalog';
    this.formAuthNamespace = spec.authSecret?.namespace || '';
    this.formAuthPath = spec.authSecret?.path || '';
    this.formKeys = (spec.trustedKeys || []).join('\n\n');
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

  @action setDisplay(v) {
    this.formDisplay = v;
  }

  @action setAuthNamespace(v) {
    this.formAuthNamespace = v;
  }

  @action setAuthPath(v) {
    this.formAuthPath = v;
  }

  @action setKeys(v) {
    this.formKeys = v;
  }

  // The spec as written, keeping what the form does not edit (the
  // selection).
  get formSpec() {
    const spec = { ...(this.editing?.registry?.spec || {}) };
    spec.type = this.formType;
    spec.source = this.formSource.trim();
    spec.display = this.formDisplay;
    const path = this.formAuthPath.trim();
    if (path) {
      spec.authSecret = { path };
      if (this.formAuthNamespace.trim())
        spec.authSecret.namespace = this.formAuthNamespace.trim();
    } else {
      delete spec.authSecret;
    }
    const keys = this.formKeys
      .split(/\n\s*\n/)
      .map((k) => k.trim())
      .filter(Boolean);
    if (keys.length) spec.trustedKeys = keys;
    else delete spec.trustedKeys;
    return spec;
  }

  @action async save() {
    const name = this.formName.trim();
    if (!NAME.test(name)) {
      this.formError = 'The name is lowercase letters, digits and dashes.';
      return;
    }
    if (!this.formSource.trim()) {
      this.formError = 'The source is required.';
      return;
    }
    this.isSaving = true;
    this.formError = null;
    try {
      await waitForPromise(
        applyRiggerRegistry(
          this.token,
          this.region,
          name,
          this.formSpec,
          this.editing.registry?.modifyIndex
        )
      );
      this.editing = null;
      await this.sync({ name });
    } catch (e) {
      this.formError = errorMessage(e);
    } finally {
      this.isSaving = false;
    }
  }

  @action askDelete(row) {
    this.toDelete = row;
  }

  @action cancelDelete() {
    this.toDelete = null;
  }

  @action async confirmDelete() {
    this.isDeleting = true;
    try {
      await waitForPromise(
        deleteRiggerRegistry(this.token, this.region, this.toDelete.name)
      );
      this.toDelete = null;
      await this.load();
    } catch (e) {
      this.notifications.add({
        title: 'The registry could not be removed',
        message: errorMessage(e),
        color: 'critical',
      });
    } finally {
      this.isDeleting = false;
    }
  }
}
