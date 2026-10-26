/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { listKinds, listObjects, kindId } from 'nomad-ui/utils/objects';
import { listReleases } from 'nomad-ui/utils/releases';
import LocalList from 'nomad-ui/utils/local-list';

// The release that registered each kind, from the resources of the
// releases ("object-kind" resources named group/Kind).
export function kindOwners(releases) {
  const owners = new Map();
  for (const r of releases || []) {
    for (const res of r.Status?.Resources || []) {
      if (res.Type === 'object-kind')
        owners.set(res.Name, { name: r.Name, namespace: r.Namespace });
    }
  }
  return owners;
}

/**
 * The type definitions (object kinds): scope, version, rules, columns,
 * scaling, webhooks, the number of objects and the package that brought
 * the kind.
 */
export default class KindsList extends Component {
  @service token;
  @service system;

  @tracked kinds = [];
  @tracked counts = {};
  @tracked owners = new Map();
  @tracked isLoading = true;
  @tracked error = null;

  @service router;

  @tracked editing = false;

  get canWrite() {
    return allowsBlock(this.token, 'Operator', 'write');
  }

  @action openEditor() {
    this.editing = true;
  }

  @action closeEditor() {
    this.editing = false;
  }

  @action created(doc) {
    this.editing = false;
    this.router.transitionTo('kinds.kind', doc.Group, doc.Kind);
  }

  get region() {
    return this.system.activeRegion || '';
  }

  list = new LocalList({
    sortBy: 'id',
    searchProps: ['id', 'Description', 'packageName'],
  });

  columns = [
    { key: 'id', label: 'Type', sortable: true, hideable: false },
    { key: 'scope', label: 'Scope', sortable: true },
    { key: 'version', label: 'Version', sortable: true },
    { key: 'objects', label: 'Objects', sortable: true },
    { key: 'rules', label: 'Rules', sortable: true },
    { key: 'scalable', label: 'Scalable', sortable: true },
    { key: 'webhooks', label: 'Webhooks', sortable: true },
    { key: 'packageName', label: 'Package', sortable: true },
  ];

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get rows() {
    return this.kinds.map((k) => {
      const id = kindId(k);
      const owner = this.owners.get(id);
      const webhooks = k.Webhooks || [];
      return {
        ...k,
        id,
        builtIn: k.Group === 'packs.norca.io',
        scope: k.Namespaced ? 'namespace' : 'cluster',
        rules: (k.Rules || []).length,
        columns: (k.Columns || []).length,
        scalable: k.Scale ? `yes (${k.Scale.SpecPath})` : 'no',
        webhooks: webhooks.length,
        pendingWebhooks: webhooks.filter((w) => !w.Approval).length,
        objects: this.counts[id],
        owner,
        version: k.Version,
        packageName: owner?.name,
      };
    });
  }

  async load(region) {
    try {
      this.kinds = (await listKinds(this.token, region)).body || [];
      this.error = null;
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot list object kinds: it needs operator read or a capability on an object kind.'
          : errorMessage(e);
      this.isLoading = false;
      return;
    }
    this.isLoading = false;
    const [counts, releases] = await Promise.all([
      Promise.all(
        this.kinds.map(async (k) => {
          try {
            const { body } = await listObjects(
              this.token,
              region,
              k.Group,
              k.Kind,
              '*'
            );
            return [kindId(k), (body || []).length];
          } catch (e) {
            return [kindId(k), undefined];
          }
        })
      ),
      listReleases(this.token, region, '*')
        .then((r) => r.body)
        .catch(() => []),
    ]);
    if (this.isDestroying || this.isDestroyed) return;
    this.counts = Object.fromEntries(counts);
    this.owners = kindOwners(releases);
  }

  setup = modifier((element, [region]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
  });
}
