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
import { allowsNamespace } from 'nomad-ui/utils/norca-acl';
import {
  listIntentions,
  saveIntention,
  deleteIntention,
} from 'nomad-ui/utils/networking';

const ALLOW = '#2fb344';
const DENY = '#d63939';

function endpoint(namespace, name, base) {
  const ns = namespace || 'default';
  if (name === '*') return ns === '*' ? 'any service' : `any in ${ns}`;
  return ns === base ? name : `${ns}/${name}`;
}

/**
 * Service intentions: who can talk to whom by mTLS identity, as a graph
 * and a list; creating, changing and deleting them needs write-intention.
 */
export default class NetworkIntentions extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked intentions = [];
  @tracked isLoading = true;
  @tracked error = null;
  @tracked editing = null;
  @tracked isSaving = false;
  @tracked saveError = null;
  @tracked deleting = null;
  @tracked isDeleting = false;
  @tracked deleteError = null;

  get region() {
    return this.args.region;
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get baseNamespace() {
    return this.namespace === '*' ? 'default' : this.namespace;
  }

  get canWrite() {
    return allowsNamespace(this.token, this.namespace, 'write-intention');
  }

  get rows() {
    return this.intentions
      .map((i) => ({
        ...i,
        key: `${i.Namespace}/${i.Destination}/${i.SourceNamespace}/${i.Source}`,
        source: endpoint(i.SourceNamespace, i.Source, this.baseNamespace),
        destination: endpoint(i.Namespace, i.Destination, this.baseNamespace),
        canWrite: allowsNamespace(this.token, i.Namespace, 'write-intention'),
      }))
      .sort((a, b) => a.destination.localeCompare(b.destination));
  }

  get graph() {
    const names = new Set();
    this.rows.forEach((r) => {
      names.add(r.source);
      names.add(r.destination);
    });
    return {
      tooltip: { formatter: (p) => p.data.tooltip || p.name },
      series: [
        {
          type: 'graph',
          layout: 'circular',
          circular: { rotateLabel: false },
          roam: false,
          symbolSize: 18,
          label: { show: true, position: 'right' },
          edgeSymbol: ['none', 'arrow'],
          edgeSymbolSize: 9,
          lineStyle: { width: 2, curveness: 0.15 },
          data: [...names].map((name) => ({ name })),
          links: this.rows.map((r) => ({
            source: r.source,
            target: r.destination,
            tooltip: `${r.source} → ${r.destination}: ${r.Action}`,
            lineStyle: {
              color: r.Action === 'allow' ? ALLOW : DENY,
              type: r.Action === 'allow' ? 'solid' : 'dashed',
            },
          })),
        },
      ],
    };
  }

  get graphSummary() {
    return this.rows
      .map((r) => `${r.source} to ${r.destination}: ${r.Action}`)
      .join('; ');
  }

  async load() {
    try {
      const { body, index } = await listIntentions(
        this.token,
        this.region,
        this.namespace
      );
      this.intentions = body || [];
      this.error = null;
      return index;
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot read the intentions of this namespace (read-job is needed).'
          : errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [region, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['ServiceIntention'],
      region,
      namespace,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  @action
  add() {
    this.saveError = null;
    this.editing = {
      isNew: true,
      Namespace: this.baseNamespace,
      Destination: '',
      SourceNamespace: this.baseNamespace,
      Source: '',
      Action: 'allow',
      Description: '',
    };
  }

  @action
  edit(row) {
    this.saveError = null;
    this.editing = {
      isNew: false,
      Namespace: row.Namespace,
      Destination: row.Destination,
      SourceNamespace: row.SourceNamespace,
      Source: row.Source,
      Action: row.Action,
      Description: row.Description || '',
    };
  }

  @action
  setField(field, value) {
    this.editing = { ...this.editing, [field]: value };
  }

  @action
  closeEditor() {
    if (!this.isSaving) this.editing = null;
  }

  @action
  async save(event) {
    event?.preventDefault();
    const e = this.editing;
    if (!e.Source.trim() || !e.Destination.trim()) {
      this.saveError = 'Give a source and a destination service, or * for any.';
      return;
    }
    this.isSaving = true;
    this.saveError = null;
    try {
      // eslint-disable-next-line no-unused-vars
      const { isNew, ...intention } = e;
      intention.Source = intention.Source.trim();
      intention.Destination = intention.Destination.trim();
      await saveIntention(this.token, this.region, intention);
      this.editing = null;
      await this.load();
    } catch (err) {
      this.saveError = errorMessage(err);
    } finally {
      this.isSaving = false;
    }
  }

  @action
  askDelete(row) {
    this.deleteError = null;
    this.deleting = row;
  }

  @action
  cancelDelete() {
    this.deleting = null;
  }

  @action
  async confirmDelete() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deleteIntention(this.token, this.region, this.deleting);
      this.deleting = null;
      await this.load();
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }
}
