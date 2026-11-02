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
import { allowsBlock, allowsNamespace } from 'nomad-ui/utils/norca-acl';
import {
  getPolicy,
  policyStatus,
  deletePolicy,
  ruleRows,
  selectorText,
} from 'nomad-ui/utils/networking';

/**
 * A network policy (@admin false, with @namespace) or an admin network
 * policy: what it applies to, its rules, and on which allocations it is
 * enforced (an allocation whose network mode cannot be filtered is listed
 * with the reason).
 */
export default class NetworkPolicy extends Component {
  @service token;
  @service system;
  @service router;
  @service notifications;

  @tracked policy = null;
  @tracked status = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked confirming = false;
  @tracked isDeleting = false;
  @tracked deleteError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get ref() {
    return {
      admin: !!this.args.admin,
      name: this.args.name,
      namespace: this.args.namespace,
    };
  }

  get canDelete() {
    return this.args.admin
      ? allowsBlock(this.token, 'Operator', 'write')
      : allowsNamespace(
          this.token,
          this.args.namespace,
          'write-network-policy'
        );
  }

  get details() {
    const p = this.policy;
    if (!p) return [];
    const items = [
      { title: 'Applies to', value: selectorText(p.Selector) },
      {
        title: 'Enforcement',
        value:
          p.Enforcement === 'best_effort'
            ? 'best effort: allocations that cannot be filtered still run'
            : 'required: allocations that cannot be filtered are not placed',
      },
    ];
    if (this.args.admin) {
      items.unshift({ title: 'Priority', value: p.Priority });
      items.push({
        title: 'Namespaces',
        value: (p.Namespaces || []).join(', ') || 'all',
      });
    } else {
      items.push({
        title: 'Default deny',
        value: (p.DefaultDeny || []).join(', ') || 'none',
      });
    }
    return items;
  }

  get rules() {
    return ruleRows(this.policy);
  }

  get statusRows() {
    return this.status.map((s) => ({
      ...s,
      shortId: s.ID.split('-')[0],
    }));
  }

  get notEnforced() {
    return this.status.filter((s) => !s.Enforceable).length;
  }

  async load() {
    try {
      const [policy, status] = await Promise.all([
        getPolicy(this.token, this.region, this.ref),
        policyStatus(this.token, this.region, this.ref).catch(() => ({
          body: [],
        })),
      ]);
      this.policy = policy.body;
      this.status = status.body || [];
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The policy does not exist. It may have been deleted.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier(() => {
    waitForPromise(Promise.resolve().then(() => this.load()));
  });

  @action ask() {
    this.deleteError = null;
    this.confirming = true;
  }

  @action cancel() {
    this.confirming = false;
  }

  @action
  async delete() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deletePolicy(this.token, this.region, this.ref);
      this.notifications.add({
        title: `Network policy ${this.args.name} deleted`,
        color: 'success',
      });
      this.confirming = false;
      this.router.transitionTo('network', {
        queryParams: { tab: this.args.admin ? 'admin' : 'policies' },
      });
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }
}
