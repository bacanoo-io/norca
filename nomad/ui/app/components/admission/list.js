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
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import { matchText, denialItem } from 'nomad-ui/utils/admission';
import LocalList from 'nomad-ui/utils/local-list';

/**
 * Admission policies (K8s item 2): CEL rules that deny or warn about job,
 * Variables and release writes, the webhooks of the server configuration,
 * and the denials seen while the page is open.
 */
export default class AdmissionList extends Component {
  @service token;
  @service system;
  @service eventStream;
  @service abilities;

  @tracked policies = [];
  @tracked webhooks = null;
  @tracked denials = [];
  @tracked isLoading = true;
  @tracked error = null;
  @tracked editing = false;

  @service router;

  get canWrite() {
    return allowsBlock(this.token, 'Admission', 'write');
  }

  @action openEditor() {
    this.editing = true;
  }

  @action closeEditor() {
    this.editing = false;
  }

  @action created(name) {
    this.editing = false;
    this.router.transitionTo('admission.policy', name);
  }

  get region() {
    return this.system.activeRegion || '';
  }

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'Description', 'match'],
  });

  columns = [
    { key: 'name', label: 'Policy', sortable: true, hideable: false },
    { key: 'action', label: 'Action', sortable: true },
    { key: 'match', label: 'Applies to', sortable: true },
    { key: 'rules', label: 'Rules', sortable: true },
  ];

  get tableRows() {
    return this.list.rows(this.rows);
  }

  get rows() {
    return this.policies
      .map((p) => ({
        ...p,
        name: p.Name,
        action: p.Action || 'deny',
        match: matchText(p.Match),
        rules: (p.Rules || []).length,
        keys: (p.TrustedKeys || []).length,
      }))
      .sort((a, b) => a.Name.localeCompare(b.Name));
  }

  async load(region) {
    try {
      this.policies =
        (
          await apiRequest(this.token, '/v1/admission-policies', {
            params: { region },
          })
        ).body || [];
      this.error = null;
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot read admission policies. Ask for a policy with admission { policy = "read" }.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
    try {
      const { body } = await apiRequest(this.token, '/v1/agent/self', {
        params: { region },
      });
      const list = body?.config?.Server?.Admission?.Webhooks;
      this.webhooks = Array.isArray(list) ? list : [];
    } catch (e) {
      this.webhooks = null;
    }
  }

  setup = modifier((element, [region]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['AdmissionPolicy'],
      region,
      onEvents: (events) => {
        const denied = events.filter((e) =>
          ['JobAdmissionDenied', 'VariableAdmissionDenied'].includes(e.Type)
        );
        if (denied.length)
          this.denials = [...denied.map(denialItem), ...this.denials].slice(
            0,
            30
          );
        if (events.some((e) => e.Type.startsWith('AdmissionPolicy')))
          waitForPromise(this.load(region));
      },
      onResync: () => this.load(region),
    });
    return () => unsubscribe();
  });
}
