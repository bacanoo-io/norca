/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  listNetworkPolicies,
  listAdminPolicies,
  selectorText,
} from 'nomad-ui/utils/networking';

/**
 * Network policies of the namespaces in scope (@admin false) or the admin
 * network policies of the cluster, by priority (@admin true).
 */
export default class NetworkPolicies extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked policies = [];
  @tracked isLoading = true;
  @tracked error = null;

  get namespace() {
    return this.frame.namespace || '*';
  }

  get columns() {
    const columns = [{ key: 'name', label: 'Policy' }];
    if (this.args.admin)
      columns.push(
        { key: 'priority', label: 'Priority' },
        { key: 'namespaces', label: 'Namespaces' }
      );
    else columns.push({ key: 'namespace', label: 'Namespace' });
    columns.push(
      { key: 'selector', label: 'Applies to' },
      { key: 'rules', label: 'Rules' },
      { key: 'enforcement', label: 'Enforcement' }
    );
    return columns;
  }

  get rows() {
    return this.policies.map((p) => ({
      ...p,
      selector: selectorText(p.Selector),
      ingress: (p.Ingress || []).length,
      egress: (p.Egress || []).length,
      defaultDeny: (p.DefaultDeny || []).join(', '),
      namespaces: (p.Namespaces || []).join(', ') || 'all',
    }));
  }

  get emptyMessage() {
    return this.args.admin
      ? 'No admin network policy. They set cluster-wide rules that namespace policies cannot override; operator write is needed to create them.'
      : 'No network policy in scope: allocations accept every connection their network mode allows. Add a policy with the network-policy CLI or API.';
  }

  async load() {
    try {
      const { body, index } = this.args.admin
        ? await listAdminPolicies(this.token, this.args.region)
        : await listNetworkPolicies(
            this.token,
            this.args.region,
            this.namespace
          );
      this.policies = body || [];
      this.error = null;
      return index;
    } catch (e) {
      this.error =
        e.status === 403
          ? this.args.admin
            ? 'Admin network policies need operator read.'
            : 'Your token cannot read the network policies of this namespace.'
          : errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [region, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['NetworkPolicy'],
      region,
      namespace,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });
}
