/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { allowsBlock, isUnrestricted } from 'nomad-ui/utils/norca-acl';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getKind,
  listObjects,
  approveWebhook,
  deleteKind,
  prettySchema,
} from 'nomad-ui/utils/objects';
import { listReleases } from 'nomad-ui/utils/releases';
import { formatNs } from 'nomad-ui/utils/scaling';
import { kindOwners } from 'nomad-ui/components/kinds/list';

/**
 * A type definition: schema version, schema, CEL rules, list columns, the
 * scale subresource, type-bound webhooks with their approval (a management
 * token approves or revokes them), write limits, the migration of the
 * stored objects between versions and the release that installed it. Live
 * from the CustomObject topic of the kind.
 */
export default class KindsDetail extends Component {
  @service token;
  @service system;
  @service eventStream;
  @service router;
  @service notifications;

  @tracked kind = null;
  @tracked owner = null;
  @tracked objectCount = null;
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked editing = false;

  // the webhook being approved or revoked: { webhook, revoke }
  @tracked approval = null;
  @tracked approvalError = null;
  @tracked isApproving = false;

  @tracked deleting = false;
  @tracked deleteError = null;
  @tracked isDeleting = false;

  get builtIn() {
    return this.args.group === 'packs.norca.io';
  }

  // Kinds are written with operator write; the built-in kinds of the
  // packs.norca.io group are read-only.
  get canWrite() {
    return !this.builtIn && allowsBlock(this.token, 'Operator', 'write');
  }

  // Webhook approvals need a management token (or ACLs disabled).
  get canApprove() {
    return !this.builtIn && isUnrestricted(this.token);
  }

  @action openEditor() {
    this.editing = true;
  }

  @action closeEditor() {
    this.editing = false;
  }

  @action
  async saved() {
    this.editing = false;
    await this.load(this.region);
  }

  get region() {
    return this.system.activeRegion || '';
  }

  get id() {
    return `${this.args.group}/${this.args.kind}`;
  }

  get details() {
    const k = this.kind;
    if (!k) return [];
    const items = [
      { title: 'Scope', value: k.Namespaced ? 'namespace' : 'cluster' },
      { title: 'Schema version', value: `v${k.Version || 1}` },
      {
        title: 'Scale subresource',
        value: k.Scale
          ? `${k.Scale.SpecPath} (status ${k.Scale.StatusPath || '–'})`
          : 'none',
      },
      {
        title: 'Objects',
        value: this.objectCount === null ? '–' : String(this.objectCount),
      },
    ];
    return items;
  }

  // Every limit of the kind, or none: no limit without a limits block.
  get limits() {
    const l = this.kind?.Limits;
    if (!l) return [];
    const rate = (r, b) => (r ? `${r} per second, burst ${b || 1}` : null);
    return [
      { title: 'Writes of the kind', value: rate(l.WriteRate, l.WriteBurst) },
      {
        title: 'Writes per identity',
        value: rate(l.IdentityWriteRate, l.IdentityWriteBurst),
      },
      {
        title:
          l.MaxObjectsPerNamespace && this.kind.Namespaced
            ? 'Objects per namespace'
            : 'Objects',
        value: l.MaxObjectsPerNamespace
          ? `at most ${l.MaxObjectsPerNamespace}`
          : null,
      },
      {
        title: 'Watches per identity',
        value: l.MaxWatchesPerIdentity
          ? `at most ${l.MaxWatchesPerIdentity} blocking queries per server`
          : null,
      },
    ].filter((i) => i.value);
  }

  get schema() {
    return prettySchema(this.kind?.Schema);
  }

  get webhooks() {
    return (this.kind?.Webhooks || []).map((w) => ({
      ...w,
      mode: w.Mode || 'validate',
      failure: w.FailurePolicy || 'fail',
      operations: (w.Operations || []).join(', ') || 'create, update',
      timeout: w.Timeout ? formatNs(w.Timeout) : '5 s',
      approved: !!w.Approval,
      approvedBy: w.Approval?.ApprovedBy || '',
      approvedAt: w.Approval?.ApproveTime
        ? moment(w.Approval.ApproveTime / 1e6).fromNow()
        : '',
      hasCA: !!w.CACert,
    }));
  }

  get pendingWebhooks() {
    return this.webhooks.filter((w) => !w.approved).length;
  }

  get migration() {
    const m = this.kind?.Migration;
    if (!m) return null;
    return [m.Spec && `spec: ${m.Spec}`, m.Status && `status: ${m.Status}`]
      .filter(Boolean)
      .join('\n');
  }

  async load(region) {
    try {
      this.kind = (
        await getKind(this.token, region, this.args.group, this.args.kind)
      ).body;
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The type definition does not exist. It may have been removed with its package.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
    if (!this.kind) return;
    const [count, releases] = await Promise.all([
      listObjects(this.token, region, this.args.group, this.args.kind, '*')
        .then((r) => (r.body || []).length)
        .catch(() => null),
      listReleases(this.token, region, '*')
        .then((r) => r.body)
        .catch(() => []),
    ]);
    if (this.isDestroying || this.isDestroyed) return;
    this.objectCount = count;
    this.owner = kindOwners(releases).get(this.id) || null;
  }

  @action
  openApproval(webhook, revoke) {
    this.approvalError = null;
    this.approval = { webhook, revoke };
  }

  @action
  closeApproval() {
    this.approval = null;
  }

  @action
  async confirmApproval() {
    const { webhook, revoke } = this.approval;
    this.isApproving = true;
    this.approvalError = null;
    try {
      const { body } = await approveWebhook(
        this.token,
        this.region,
        this.args.group,
        this.args.kind,
        webhook.Name,
        revoke
      );
      if (body) this.kind = body;
      this.approval = null;
      this.notifications.add({
        title: `Webhook ${webhook.Name} ${revoke ? 'revoked' : 'approved'}`,
        message: revoke
          ? 'It is no longer called; writes it reviews fail unless its failure policy is ignore.'
          : 'It is called for the writes it reviews from now on.',
        color: 'success',
      });
      await this.load(this.region);
    } catch (e) {
      this.approvalError =
        e.status === 403
          ? 'Only a management token can approve or revoke webhooks.'
          : errorMessage(e);
    } finally {
      this.isApproving = false;
    }
  }

  @action
  openDelete() {
    this.deleteError = null;
    this.deleting = true;
  }

  @action
  closeDelete() {
    this.deleting = false;
  }

  @action
  async confirmDelete() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deleteKind(
        this.token,
        this.region,
        this.args.group,
        this.args.kind
      );
      this.deleting = false;
      this.notifications.add({
        title: `Type definition ${this.id} deleted`,
        color: 'success',
      });
      this.router.transitionTo('kinds.index');
    } catch (e) {
      this.deleteError =
        e.status === 403
          ? 'Your token cannot delete type definitions: it needs operator write.'
          : errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }

  setup = modifier((element, [region, id]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`CustomObject:${id}`],
      region,
      namespace: '*',
      onEvents: (events) => {
        // only changes of the kind itself (key group/Kind), not its objects
        if (events.some((e) => e.Key === id)) waitForPromise(this.load(region));
      },
      onResync: () => this.load(region),
    });
    return () => unsubscribe();
  });
}
