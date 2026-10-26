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
  objectEvents,
  conditions,
  objectHealth,
  evaluatePath,
  display,
} from 'nomad-ui/utils/objects';
import { RELEASE_KIND } from 'nomad-ui/utils/releases';

/**
 * A custom object: the spec (the user's), the status (the controller's)
 * with the kind's columns, the conditions, generation and observed
 * generation, labels, owners, finalizers, and the event log (the newest 20
 * events of the object).
 */
export default class ObjectsDetail extends Component {
  @service token;
  @service system;
  @service eventStream;

  @tracked object = null;
  @tracked kind = null;
  @tracked events = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked editing = false;

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

  get statusItems() {
    const o = this.object;
    if (!o) return [];
    const items = (this.kind?.Columns || []).map((c) => ({
      title: c.Name,
      value: display(evaluatePath(o, c.Path)),
    }));
    items.push(
      { title: 'Generation', value: o.Generation },
      {
        title: 'Observed generation',
        value: o.Status?.observedGeneration ?? 'not reported',
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
    return (this.object?.Owners || []).map((o) => ({
      ...o,
      isRelease: o.Kind === RELEASE_KIND,
    }));
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
      this.events =
        (
          await objectEvents(this.token, this.region, this.ref).catch(() => ({
            body: [],
          }))
        ).body || [];
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The object does not exist. It may have been deleted, or it is in another namespace or region.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
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
