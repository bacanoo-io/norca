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
  listKinds,
  listObjects,
  kindId,
  evaluatePath,
  display,
  objectHealth,
} from 'nomad-ui/utils/objects';

/**
 * The objects of one kind, in a table with the columns of its type
 * definition, their health (conditions) and whether the controller has seen
 * the latest generation. Live from the CustomObject topic of the kind.
 */
export default class ObjectsList extends Component {
  @service token;
  @service system;
  @service frame;
  @service eventStream;

  @tracked kinds = [];
  @tracked objects = [];
  @tracked isLoading = true;
  @tracked objectsLoading = false;
  @tracked error = null;
  @tracked objectsError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get kind() {
    const wanted = this.args.kind;
    return (
      this.kinds.find((k) => kindId(k) === wanted) ||
      this.kinds.find((k) => k.Group !== 'packs.norca.io') ||
      this.kinds[0] ||
      null
    );
  }

  get kindOptions() {
    return this.kinds.map((k) => ({ value: kindId(k), label: kindId(k) }));
  }

  get columns() {
    const k = this.kind;
    if (!k) return [];
    const columns = [{ key: 'name', label: 'Name' }];
    if (k.Namespaced) columns.push({ key: 'namespace', label: 'Namespace' });
    (k.Columns || []).forEach((c, i) =>
      columns.push({ key: `col-${i}`, label: c.Name })
    );
    columns.push(
      { key: 'health', label: 'Health' },
      { key: 'generation', label: 'Generation' }
    );
    return columns;
  }

  get rows() {
    const k = this.kind;
    return this.objects
      .map((o) => ({
        ...o,
        cells: (k?.Columns || []).map((c) => display(evaluatePath(o, c.Path))),
        health: objectHealth(o),
        generation: `${o.Generation} / ${o.Status?.observedGeneration ?? '–'}`,
      }))
      .sort((a, b) => a.Name.localeCompare(b.Name));
  }

  get scopeLabel() {
    const ns =
      this.namespace === '*' ? 'all namespaces' : `namespace ${this.namespace}`;
    return `${this.region || 'Local region'}, ${ns}`;
  }

  async loadKinds(region) {
    try {
      this.kinds = (await listKinds(this.token, region)).body || [];
      this.error = null;
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot list object kinds: it needs operator read or a capability on an object kind.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  async loadObjects() {
    const k = this.kind;
    if (!k) return 0;
    this.objectsLoading = true;
    try {
      const { body, index } = await listObjects(
        this.token,
        this.region,
        k.Group,
        k.Kind,
        this.namespace
      );
      this.objects = body || [];
      this.objectsError = null;
      return index;
    } catch (e) {
      this.objects = [];
      this.objectsError =
        e.status === 403
          ? `Your token cannot read ${kindId(k)} objects in this namespace.`
          : errorMessage(e);
      return 0;
    } finally {
      this.objectsLoading = false;
    }
  }

  setup = modifier((element, [region]) => {
    waitForPromise(Promise.resolve().then(() => this.loadKinds(region)));
  });

  objectsLoader = modifier((element, [id, namespace]) => {
    if (!id) return;
    waitForPromise(Promise.resolve().then(() => this.loadObjects()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`CustomObject:${id}`],
      region: this.region,
      namespace,
      onEvents: () => waitForPromise(this.loadObjects()),
      onResync: () => this.loadObjects(),
    });
    return () => unsubscribe();
  });

  @action
  selectKind(value) {
    this.args.onKindChange?.(value);
  }
}
