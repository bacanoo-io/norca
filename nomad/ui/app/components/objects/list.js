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
import LocalList from 'nomad-ui/utils/local-list';
import { allowsObject } from 'nomad-ui/utils/norca-acl';

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
  @service router;

  @tracked creating = false;

  // New objects go to the namespace in scope, or default for all.
  get targetNamespace() {
    return this.namespace === '*' ? 'default' : this.namespace;
  }

  get canCreate() {
    const k = this.kind;
    if (!k || k.Group === 'packs.norca.io') return false;
    return allowsObject(
      this.token,
      this.targetNamespace,
      `${k.Group}/${k.Kind}`,
      'write'
    );
  }

  @action openCreate() {
    this.creating = true;
  }

  @action closeCreate() {
    this.creating = false;
  }

  @action created(object) {
    this.creating = false;
    this.router.transitionTo(
      'objects.object',
      object.Group,
      object.Kind,
      object.Name,
      { queryParams: { namespace: object.Namespace || null } }
    );
  }

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
    const columns = [
      { key: 'name', label: 'Name', sortable: true, hideable: false },
    ];
    if (k.Namespaced)
      columns.push({ key: 'namespace', label: 'Namespace', sortable: true });
    (k.Columns || []).forEach((c, i) =>
      columns.push({ key: `col-${i}`, label: c.Name, sortable: true })
    );
    if (k.Scale)
      columns.push({ key: 'scale', label: 'Scale (desired / current)' });
    columns.push(
      { key: 'healthLabel', label: 'Health', sortable: true },
      { key: 'generation', label: 'Generation' }
    );
    return columns;
  }

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'namespace', 'healthLabel'],
  });

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get rows() {
    const k = this.kind;
    return this.objects
      .map((o) => {
        const cells = (k?.Columns || []).map((c, i) => ({
          key: `col-${i}`,
          value: display(evaluatePath(o, c.Path)),
        }));
        const health = objectHealth(o);
        return {
          ...o,
          ...Object.fromEntries(cells.map((c) => [c.key, c.value])),
          name: o.Name,
          namespace: o.Namespace,
          cells,
          health,
          healthLabel: health.label,
          scale: k?.Scale
            ? `${display(evaluatePath(o, k.Scale.SpecPath))} / ${display(
                k.Scale.StatusPath
                  ? evaluatePath(o, k.Scale.StatusPath)
                  : undefined
              )}`
            : '',
          generation: `${o.Generation} / ${
            o.Status?.observedGeneration ?? '–'
          }`,
        };
      })
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
