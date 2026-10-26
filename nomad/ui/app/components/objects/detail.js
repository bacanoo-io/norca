/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { allowsObject } from 'nomad-ui/utils/norca-acl';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getObject,
  getKind,
  getScale,
  listKinds,
  listObjects,
  listOwnedJobs,
  objectEvents,
  deleteObject,
  conditions,
  objectHealth,
  evaluatePath,
  display,
  ownerLink,
  ownsObject,
  deletionState,
  DELETION_MODES,
} from 'nomad-ui/utils/objects';

/**
 * A custom object: the spec (the user's), the status (the controller's)
 * with the kind's columns, the conditions, generation and observed
 * generation, the schema version it is stored at, the scale subresource,
 * labels, owners and what the object owns (jobs and objects), finalizers,
 * the deletion with its mode, and the event log (the newest 20 events of
 * the object). Live from the CustomObject topic of the object.
 */
export default class ObjectsDetail extends Component {
  @service token;
  @service system;
  @service eventStream;
  @service router;
  @service notifications;

  @tracked object = null;
  @tracked kind = null;
  @tracked events = [];
  @tracked scale = null;
  @tracked ownedJobs = null;
  @tracked ownedObjects = null;
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked editing = false;

  @tracked deleting = false;
  @tracked deletionMode = 'background';
  @tracked deleteError = null;
  @tracked isDeleting = false;

  deletionModes = DELETION_MODES;

  get canWrite() {
    return allowsObject(
      this.token,
      this.object?.Namespace || this.args.namespace,
      this.kindId,
      'write'
    );
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
    await this.load();
  }

  get region() {
    return this.system.activeRegion || '';
  }

  get ref() {
    return {
      group: this.args.group,
      kind: this.args.kind,
      name: this.args.name,
      namespace: this.args.namespace,
    };
  }

  get kindId() {
    return `${this.args.group}/${this.args.kind}`;
  }

  get health() {
    return this.object ? objectHealth(this.object) : null;
  }

  get deletion() {
    return deletionState(this.object);
  }

  get deletionModeHelp() {
    return DELETION_MODES.find((m) => m.value === this.deletionMode)?.help;
  }

  // The schema version the object is stored at (0: stored before schema
  // versions, which is version 1) against the version of its kind.
  get kindVersion() {
    const o = this.object;
    if (!o) return null;
    const stored = o.KindVersion || 1;
    const current = this.kind?.Version || null;
    return {
      stored,
      current,
      behind: current !== null && stored < current,
    };
  }

  get statusItems() {
    const o = this.object;
    if (!o) return [];
    const items = (this.kind?.Columns || []).map((c) => ({
      title: c.Name,
      value: display(evaluatePath(o, c.Path)),
    }));
    const v = this.kindVersion;
    items.push(
      { title: 'Generation', value: o.Generation },
      {
        title: 'Observed generation',
        value: o.Status?.observedGeneration ?? 'not reported',
      },
      {
        title: 'Schema version',
        value: v.behind
          ? `v${v.stored} (the kind is at v${v.current}, migration pending)`
          : `v${v.stored}`,
      },
      { title: 'Managed by', value: o.ManagedBy || 'anyone with write' },
      {
        title: 'Changed',
        value: o.ModifyTime ? moment(o.ModifyTime / 1e6).fromNow() : '–',
      }
    );
    if (o.DeletionTime)
      items.push({
        title: 'Deleting since',
        value: moment(o.DeletionTime / 1e6).fromNow(),
      });
    return items;
  }

  get scaleItems() {
    const s = this.scale;
    if (!s) return [];
    return [
      { title: 'Desired', value: `${s.Replicas} (${s.SpecPath})` },
      {
        title: 'Current',
        value:
          s.StatusReplicas === null || s.StatusReplicas === undefined
            ? `not reported${s.StatusPath ? ` (${s.StatusPath})` : ''}`
            : `${s.StatusReplicas} (${s.StatusPath})`,
      },
    ];
  }

  get scaleConverging() {
    const s = this.scale;
    return (
      s &&
      s.StatusReplicas !== null &&
      s.StatusReplicas !== undefined &&
      s.StatusReplicas !== s.Replicas
    );
  }

  get conditionRows() {
    return conditions(this.object).map((c) => {
      const status = String(c.status ?? c.Status);
      const type = c.type || c.Type;
      const bad = type === 'Degraded' ? status === 'True' : status === 'False';
      return {
        type,
        status,
        reason: c.reason || c.Reason || '–',
        message: c.message || c.Message || '',
        since: c.lastTransitionTime
          ? moment(c.lastTransitionTime).fromNow()
          : '',
        tone: bad ? 'danger' : status === 'True' ? 'success' : 'neutral',
      };
    });
  }

  get specText() {
    return this.object?.Spec || {};
  }

  get statusText() {
    // the status without the conditions, which have their own table
    // eslint-disable-next-line no-unused-vars
    const { conditions: c, ...rest } = this.object?.Status || {};
    return rest;
  }

  get labels() {
    return Object.entries(this.object?.Labels || {}).map(
      ([k, v]) => `${k}=${v}`
    );
  }

  get owners() {
    return (this.object?.Owners || []).map((o) => ownerLink(o, this.object));
  }

  get ownedJobRows() {
    return (this.ownedJobs || []).map((j) => ({
      name: j.Name || j.ID,
      routeId: `${j.ID}@${j.Namespace || 'default'}`,
      namespace: j.Namespace,
      status: j.Status,
      type: j.Type,
    }));
  }

  get ownedObjectRows() {
    return (this.ownedObjects || []).map((o) => ({
      ...o,
      kindId: `${o.Group}/${o.Kind}`,
      health: objectHealth(o),
    }));
  }

  get ownsAnything() {
    return this.ownedJobRows.length + this.ownedObjectRows.length > 0;
  }

  get eventItems() {
    return this.events
      .slice()
      .sort((a, b) => (b.LastTime || 0) - (a.LastTime || 0))
      .map((e) => ({
        icon: e.Type === 'Warning' ? 'alert-triangle' : 'circle-check',
        color: e.Type === 'Warning' ? 'orange' : 'green',
        title: e.Count > 1 ? `${e.Reason} (${e.Count}×)` : e.Reason,
        text: [e.Message, e.Reporter && `by ${e.Reporter}`]
          .filter(Boolean)
          .join(', '),
        time: e.LastTime ? moment(e.LastTime / 1e6).fromNow() : '',
      }));
  }

  async load() {
    try {
      const [object, kind] = await Promise.all([
        getObject(this.token, this.region, this.ref),
        getKind(this.token, this.region, this.args.group, this.args.kind)
          .then((r) => r.body)
          .catch(() => null),
      ]);
      this.object = object.body;
      this.kind = kind;
      const [events, scale] = await Promise.all([
        objectEvents(this.token, this.region, this.ref)
          .then((r) => r.body || [])
          .catch(() => []),
        kind?.Scale
          ? getScale(this.token, this.region, this.ref)
              .then((r) => r.body)
              .catch(() => null)
          : null,
      ]);
      this.events = events;
      this.scale = scale;
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The object does not exist. It may have been deleted, or it is in another namespace or region.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
    if (this.object) await this.loadOwned();
  }

  // What the object owns: the jobs whose meta references it, and the
  // objects of any kind whose owner references point at it. Both are
  // optional: without permission they are left out.
  async loadOwned() {
    const parent = this.object;
    const ref = {
      group: parent.Group,
      kind: parent.Kind,
      name: parent.Name,
      namespace: parent.Namespace,
    };
    const [jobs, objects] = await Promise.all([
      listOwnedJobs(this.token, this.region, ref)
        .then((r) => r.body || [])
        .catch(() => null),
      listKinds(this.token, this.region)
        .then(async ({ body }) => {
          const lists = await Promise.all(
            (body || []).map((k) =>
              listObjects(this.token, this.region, k.Group, k.Kind, '*')
                .then((r) => r.body || [])
                .catch(() => [])
            )
          );
          return lists
            .flat()
            .filter((o) =>
              (o.Owners || []).some((owner) => ownsObject(owner, o, parent))
            );
        })
        .catch(() => null),
    ]);
    if (this.isDestroying || this.isDestroyed) return;
    this.ownedJobs = jobs;
    this.ownedObjects = objects;
  }

  @action
  openDelete() {
    this.deletionMode = 'background';
    this.deleteError = null;
    this.deleting = true;
  }

  @action
  closeDelete() {
    this.deleting = false;
  }

  @action
  setDeletionMode(mode) {
    this.deletionMode = mode;
  }

  @action
  async confirmDelete() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      const { body } = await deleteObject(this.token, this.region, this.ref, {
        propagation: this.deletionMode,
        cas: this.object.ModifyIndex,
      });
      this.deleting = false;
      if (body?.DeletionTime) {
        this.notifications.add({
          title: `${this.args.kind} ${this.args.name} is being deleted`,
          message:
            'It disappears when its finalizers are removed and, in the foreground mode, what it owns is deleted.',
          color: 'success',
        });
        await this.load();
      } else {
        this.notifications.add({
          title: `${this.args.kind} ${this.args.name} deleted`,
          color: 'success',
        });
        this.router.transitionTo('objects.index', {
          queryParams: { kind: this.kindId },
        });
      }
    } catch (e) {
      this.deleteError =
        e.status === 403
          ? `Your token cannot delete ${
              this.kindId
            } objects here (object write), or a webhook or the managing identity rejected it: ${errorMessage(
              e
            )}`
          : e.status === 409
          ? 'The object changed since the page loaded it. Close this dialog, check the object and delete it again.'
          : errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }

  setup = modifier((element, [kindId, name, namespace, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`CustomObject:${kindId}/${name}`],
      region,
      namespace: namespace || '*',
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });
}
