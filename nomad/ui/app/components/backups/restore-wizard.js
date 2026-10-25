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
  restoreBackup,
  previewCounts,
  statusTone,
  itemKey,
  itemLabel,
} from 'nomad-ui/utils/backups';

const SELECTIVE_STEPS = ['Mode', 'Items', 'Preview', 'Confirm', 'Result'];
const FULL_STEPS = ['Mode', 'Preview', 'Confirm', 'Approval'];

/**
 * The restore wizard (backup-restore-plan.md, "Restore wizard"): choose the
 * mode, the items (selective), a mandatory preview computed by the server
 * without touching the cluster, the confirmation and the result.
 *
 * Selective: the chosen jobs (a version kept in the backup becomes a new
 * version), Variables paths (check-and-set), namespaces and ACL objects are
 * written as normal requests with the caller's token.
 * Full: the whole region returns to the backup. It needs a management
 * token, the typed region name and the approval of a second management
 * token; the request then waits on the restore request page.
 */
export default class BackupsRestoreWizard extends Component {
  @service token;
  @service system;
  @service backups;
  @service abilities;
  @service router;

  @tracked backup = null;
  @tracked loadError = null;
  @tracked step = 0;
  @tracked mode = null;
  @tracked results = null;
  @tracked preview = null;
  @tracked overwrite = {};
  @tracked versions = {};
  @tracked isWorking = false;
  @tracked error = null;
  @tracked identityError = null;
  @tracked typed = '';
  @tracked restore = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get regionName() {
    return this.backup?.Region || this.region;
  }

  get steps() {
    return this.mode === 'full' ? FULL_STEPS : SELECTIVE_STEPS;
  }

  get stepName() {
    return this.steps[this.step];
  }

  get canRestoreFull() {
    return this.abilities.can('restore-full backup');
  }

  get items() {
    return this.backups.draftFor(this.args.backupId).map((item) => ({
      ...item,
      key: itemKey(item),
      label: itemLabel(item),
      isJob: item.Type === 'job',
    }));
  }

  // The items sent to the server: chosen versions and, for Variables entries
  // that exist now, the check index when overwriting was allowed.
  get requestItems() {
    return this.items.map(({ key, Type, Namespace, ID, Version }) => {
      const item = { Type, ID };
      if (Namespace) item.Namespace = Namespace;
      const version = this.versions[key] ?? Version;
      if (
        Type === 'job' &&
        version !== undefined &&
        version !== null &&
        version !== ''
      )
        item.Version = Number(version);
      if (Type === 'variable' && this.overwrite[key] !== undefined)
        item.CheckIndex = this.overwrite[key];
      return item;
    });
  }

  get resultRows() {
    return (this.results || []).map((r) => {
      const key = itemKey(r.Item || {});
      const existing =
        r.Item?.Type === 'variable' && r.Action === 'update' && !r.Error;
      return {
        ...r,
        key,
        label: itemLabel(r.Item || {}),
        tone: r.Error ? 'danger' : r.Action === 'none' ? 'neutral' : 'success',
        needsOverwrite: existing,
        overwriting: this.overwrite[key] !== undefined,
      };
    });
  }

  get previewHasErrors() {
    return (this.results || []).some((r) => r.Error);
  }

  get blockedVariables() {
    return this.resultRows.filter((r) => r.needsOverwrite && !r.overwriting);
  }

  get previewRows() {
    return previewCounts(this.preview);
  }

  get previewObjects() {
    return (this.preview?.Objects || []).slice(0, 100);
  }

  get confirmMatches() {
    return this.typed.trim() === this.regionName;
  }

  get resultSummary() {
    const failed = (this.results || []).filter((r) => r.Error).length;
    return { failed, total: (this.results || []).length };
  }

  get restoreTone() {
    return statusTone(this.restore?.Status);
  }

  async load() {
    try {
      const { body } = await getBackup(
        this.token,
        this.region,
        this.args.backupId
      );
      this.backup = body;
      if (this.args.mode === 'full' || this.args.mode === 'selective') {
        this.mode = this.args.mode;
        this.step = 1;
      }
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The backup does not exist in this region. Pick the region in the top bar or go back to the list.'
          : errorMessage(e);
    }
  }

  setup = modifier(() => {
    waitForPromise(Promise.resolve().then(() => this.load()));
  });

  handleError(e) {
    const message = errorMessage(e);
    if (/identity|recipient|decrypt/i.test(message)) {
      this.backups.forgetIdentity();
      this.identityError = message;
    } else {
      this.error = message;
    }
  }

  @action
  chooseMode(mode) {
    this.mode = mode;
    this.error = null;
    this.results = null;
    this.preview = null;
    this.step = 1;
  }

  @action
  back() {
    this.error = null;
    if (this.step > 0) this.step -= 1;
    if (this.step === 0) this.mode = null;
  }

  @action
  removeItem(item) {
    this.backups.setDraft(
      this.args.backupId,
      this.backups
        .draftFor(this.args.backupId)
        .filter((i) => itemKey(i) !== item.key)
    );
  }

  @action
  setVersion(item, value) {
    this.versions = { ...this.versions, [item.key]: value };
  }

  @action
  toggleOverwrite(row, checked) {
    const overwrite = { ...this.overwrite };
    if (checked) overwrite[row.key] = row.CurrentIndex;
    else delete overwrite[row.key];
    this.overwrite = overwrite;
  }

  @action
  async previewSelective() {
    this.isWorking = true;
    this.error = null;
    try {
      const { body } = await restoreBackup(this.token, {
        region: this.region,
        id: this.args.backupId,
        Mode: 'selective',
        Items: this.requestItems,
        DryRun: true,
        AgeIdentity: this.backups.identity,
      });
      this.results = body?.Results || [];
      this.step = 2;
    } catch (e) {
      this.handleError(e);
    } finally {
      this.isWorking = false;
    }
  }

  @action
  toConfirm() {
    this.step = 3;
  }

  @action
  async runSelective() {
    this.isWorking = true;
    this.error = null;
    try {
      const { body } = await restoreBackup(this.token, {
        region: this.region,
        id: this.args.backupId,
        Mode: 'selective',
        Items: this.requestItems,
        DryRun: false,
        AgeIdentity: this.backups.identity,
      });
      this.results = body?.Results || [];
      this.restore = body?.Restore || null;
      this.backups.clearDraft(this.args.backupId);
      this.step = 4;
    } catch (e) {
      this.handleError(e);
    } finally {
      this.isWorking = false;
    }
  }

  @action
  async previewFull() {
    this.isWorking = true;
    this.error = null;
    try {
      const { body } = await restoreBackup(this.token, {
        region: this.region,
        id: this.args.backupId,
        Mode: 'full',
        DryRun: true,
        AgeIdentity: this.backups.identity,
      });
      this.preview = body?.Preview || null;
    } catch (e) {
      this.handleError(e);
    } finally {
      this.isWorking = false;
    }
  }

  @action
  toFullConfirm() {
    this.typed = '';
    this.step = 2;
  }

  @action
  setTyped(value) {
    this.typed = value;
  }

  @action
  async requestFull(event) {
    event?.preventDefault();
    if (!this.confirmMatches) return;
    this.isWorking = true;
    this.error = null;
    try {
      const { body } = await restoreBackup(this.token, {
        region: this.region,
        id: this.args.backupId,
        Mode: 'full',
        DryRun: false,
        Confirm: this.typed.trim(),
        AgeIdentity: this.backups.identity,
      });
      this.restore = body?.Restore || null;
      this.step = 3;
    } catch (e) {
      this.handleError(e);
    } finally {
      this.isWorking = false;
    }
  }

  // The full preview is loaded on entering its step (and after the identity
  // is given).
  fullPreviewLoader = modifier((element, [hasIdentity]) => {
    if (hasIdentity && !this.preview && !this.isWorking)
      waitForPromise(Promise.resolve().then(() => this.previewFull()));
  });
}
