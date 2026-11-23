/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import { normalize } from 'nomad-ui/utils/releases';
import { listRiggerOperations } from 'nomad-ui/utils/rigger';

/**
 * The stacks of Rigger in the namespace (design/rig-integration.md 4.8d):
 * those that own releases and those with an operation kept, each with its
 * releases, its source and version, and its last operation.
 */
export default class PackagesRiggerStacks extends Component {
  @service token;
  @service frame;
  @service system;

  @tracked releases = [];
  @tracked operations = [];
  @tracked isLoading = true;
  @tracked loadError = null;

  setup = modifier(() => {
    this.load();
  });

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get rows() {
    const stacks = new Map();
    const stackOf = (namespace, name) => {
      const key = `${namespace}/${name}`;
      if (!stacks.has(key)) {
        stacks.set(key, {
          key,
          namespace,
          name,
          releases: 0,
          source: '',
          version: '',
          operation: null,
        });
      }
      return stacks.get(key);
    };
    for (const r of this.releases) {
      const o = r.Spec?.Owner;
      if (!o || o.Kind !== 'stack') continue;
      const s = stackOf(o.Namespace, o.Name);
      s.releases++;
      s.source ||= o.Source || '';
      s.version ||= o.Version || '';
    }
    // The operations come the newest first: the first of a stack is its last.
    for (const op of this.operations) {
      if (!op.spec.stack) continue;
      const s = stackOf(op.namespace, op.spec.stack);
      if (!s.operation) s.operation = op;
      s.source ||= op.spec.source || '';
      s.version ||= op.spec.version || '';
    }
    return [...stacks.values()].sort((a, b) => a.key.localeCompare(b.key));
  }

  async load() {
    this.isLoading = true;
    this.loadError = null;
    try {
      const [releases, operations] = await waitForPromise(
        Promise.all([
          apiRequest(this.token, '/v1/releases', {
            params: { region: this.region, namespace: this.namespace },
          }).then((r) => normalize(r.body || [])),
          listRiggerOperations(this.token, this.region, this.namespace),
        ])
      );
      this.releases = releases;
      this.operations = operations;
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }
}
