/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action, get } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getRestore,
  approveRestore,
  rejectRestore,
  previewCounts,
  statusTone,
  identityLabel,
  isSet,
  itemLabel,
} from 'nomad-ui/utils/backups';

const POLL_MS = 5000;

/**
 * A restore record. For a full restore waiting for approval: the preview
 * the requester saw, approval by a second management token (who gives the
 * age identity again and types the region name) or rejection. Follows the
 * progress (approved, running, complete or failed) from the event stream.
 */
export default class BackupsRequest extends Component {
  @service token;
  @service system;
  @service backups;
  @service abilities;
  @service eventStream;

  @tracked restore = null;
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked approving = false;
  @tracked rejecting = false;
  @tracked isWorking = false;
  @tracked actionError = null;
  @tracked reason = '';

  get region() {
    return this.system.activeRegion || '';
  }

  get regionName() {
    return this.restore?.Preview?.Region || this.region;
  }

  get tone() {
    return statusTone(this.restore?.Status);
  }

  get isPending() {
    return this.restore?.Status === 'pending';
  }

  get isInProgress() {
    return ['approved', 'running'].includes(this.restore?.Status);
  }

  get isRequester() {
    const accessor = get(this.token, 'selfToken.id');
    return !!accessor && this.restore?.RequestedBy === `token:${accessor}`;
  }

  get canDecide() {
    return this.abilities.can('restore-full backup');
  }

  get details() {
    const r = this.restore;
    if (!r) return [];
    const items = [
      { title: 'Mode', value: r.Mode === 'full' ? 'Full region' : 'Selective' },
      { title: 'Backup', value: r.BackupID, mono: true },
      {
        title: 'Requested by',
        value: [r.RequestedByName, identityLabel(r.RequestedBy)]
          .filter(Boolean)
          .join(', '),
      },
      { title: 'Requested', value: new Date(r.CreateTime).toLocaleString() },
    ];
    if (r.Mode === 'full') {
      items.push({
        title: 'Approved by',
        value: (r.Approvals || []).map(identityLabel).join(', ') || '–',
      });
      if (isSet(r.ExpiresAt))
        items.push({
          title: r.Status === 'pending' ? 'Expires' : 'Approval deadline',
          value: new Date(r.ExpiresAt).toLocaleString(),
        });
      if (r.PreRestoreBackupID)
        items.push({
          title: 'Undo with backup',
          value: r.PreRestoreBackupID,
          mono: true,
        });
    }
    return items;
  }

  get previewRows() {
    return previewCounts(this.restore?.Preview);
  }

  get resultRows() {
    return (this.restore?.Results || []).map((r) => ({
      ...r,
      label: itemLabel(r.Item || {}),
    }));
  }

  async load() {
    try {
      const { body, index } = await getRestore(
        this.token,
        this.region,
        this.args.restoreId
      );
      this.restore = body;
      this.loadError = null;
      return index;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The restore request does not exist in this region. Pick the region in the top bar or go back to the backups.'
          : errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  live = modifier((element, [restoreId, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['Backup'],
      region,
      onEvents: (events) => {
        if (events.some((e) => e.Key === restoreId))
          waitForPromise(this.load());
      },
      onResync: () => this.load(),
    });
    let timer = null;
    if (config.environment !== 'test') {
      timer = setInterval(() => {
        if (
          (this.isPending || this.isInProgress) &&
          this.eventStream.stateFor(region, '*') !== 'open'
        )
          waitForPromise(this.load());
      }, POLL_MS);
    }
    return () => {
      unsubscribe();
      clearInterval(timer);
    };
  });

  @action
  askApprove() {
    this.actionError = null;
    this.approving = true;
  }

  @action
  askReject() {
    this.actionError = null;
    this.reason = '';
    this.rejecting = true;
  }

  @action
  closeDialogs() {
    if (this.isWorking) return;
    this.approving = false;
    this.rejecting = false;
  }

  @action
  setReason(value) {
    this.reason = value;
  }

  @action
  async approve() {
    this.isWorking = true;
    this.actionError = null;
    try {
      const { body } = await approveRestore(
        this.token,
        this.region,
        this.restore.ID,
        this.backups.identity
      );
      this.restore = body;
      this.approving = false;
    } catch (e) {
      this.actionError = errorMessage(e);
      await this.load();
    } finally {
      this.isWorking = false;
    }
  }

  @action
  async reject() {
    this.isWorking = true;
    this.actionError = null;
    try {
      const { body } = await rejectRestore(
        this.token,
        this.region,
        this.restore.ID,
        this.reason.trim()
      );
      this.restore = body;
      this.rejecting = false;
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isWorking = false;
    }
  }
}
