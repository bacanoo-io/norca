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
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';
import { matchText, denialItem } from 'nomad-ui/utils/admission';

/**
 * An admission policy: action, what it applies to, the CEL rules with
 * their messages, the schema check and trusted keys, and the denials it
 * caused while the page is open. Delete with admission write.
 */
export default class AdmissionPolicy extends Component {
  @service token;
  @service system;
  @service router;
  @service eventStream;
  @service notifications;

  @tracked policy = null;
  @tracked denials = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked confirming = false;
  @tracked isDeleting = false;
  @tracked deleteError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get canWrite() {
    return allowsBlock(this.token, 'Admission', 'write');
  }

  get details() {
    const p = this.policy;
    if (!p) return [];
    return [
      {
        title: 'Action',
        value:
          (p.Action || 'deny') === 'warn'
            ? 'warn: the write goes through with a warning'
            : 'deny: the write is rejected',
      },
      { title: 'Applies to', value: matchText(p.Match) },
      {
        title: 'Schema check',
        value:
          p.SchemaFrom === 'crd-schema'
            ? 'Variables against the object kind schema'
            : 'none',
      },
      { title: 'Trusted keys', value: (p.TrustedKeys || []).length || 'none' },
    ];
  }

  get path() {
    return `/v1/admission-policy/${encodeURIComponent(this.args.name)}`;
  }

  async load(region) {
    try {
      this.policy = (
        await apiRequest(this.token, this.path, { params: { region } })
      ).body;
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The admission policy does not exist. It may have been deleted.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [name, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['AdmissionPolicy'],
      region,
      onEvents: (events) => {
        const mine = events
          .filter((e) => e.Type.endsWith('AdmissionDenied'))
          .map(denialItem)
          .filter((d) => d.name === name);
        if (mine.length) this.denials = [...mine, ...this.denials].slice(0, 30);
        if (
          events.some(
            (e) => e.Key === name && e.Type.startsWith('AdmissionPolicy')
          )
        )
          waitForPromise(this.load(region));
      },
      onResync: () => this.load(region),
    });
    return () => unsubscribe();
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
      await apiRequest(this.token, this.path, {
        method: 'DELETE',
        params: { region: this.region },
      });
      this.notifications.add({
        title: `Admission policy ${this.args.name} deleted`,
        color: 'success',
      });
      this.confirming = false;
      this.router.transitionTo('admission.index');
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }
}
