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
import { getKind, prettySchema } from 'nomad-ui/utils/objects';
import { formatNs } from 'nomad-ui/utils/scaling';

/**
 * A type definition: schema, CEL rules, list columns, the scale
 * subresource, webhooks with their approval, write limits and the
 * migration of the stored objects between versions.
 */
export default class KindsDetail extends Component {
  @service token;
  @service system;

  @tracked kind = null;
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked editing = false;

  // Kinds are written with operator write; the built-in kinds of the
  // packs.norca.io group are read-only.
  get canWrite() {
    return (
      this.args.group !== 'packs.norca.io' &&
      allowsBlock(this.token, 'Operator', 'write')
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
      { title: 'Version', value: `v${k.Version}` },
      {
        title: 'Scale subresource',
        value: k.Scale
          ? `${k.Scale.SpecPath} (status ${k.Scale.StatusPath || '–'})`
          : 'none',
      },
    ];
    const l = k.Limits;
    if (l) {
      if (l.MaxObjectsPerNamespace)
        items.push({
          title: 'Objects per namespace',
          value: `at most ${l.MaxObjectsPerNamespace}`,
        });
      if (l.WriteRate)
        items.push({
          title: 'Writes',
          value: `${l.WriteRate}/s, burst ${l.WriteBurst}`,
        });
    }
    return items;
  }

  get schema() {
    return prettySchema(this.kind?.Schema);
  }

  get webhooks() {
    return (this.kind?.Webhooks || []).map((w) => ({
      ...w,
      operations: (w.Operations || []).join(', ') || 'all',
      timeout: w.Timeout ? formatNs(w.Timeout) : '–',
      approved: !!w.Approval,
    }));
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
  }

  setup = modifier((element, [region]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
  });
}
