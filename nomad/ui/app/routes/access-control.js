/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import notifyError from 'nomad-ui/utils/notify-error';

/**
 * Parent route of the Access Control section. It loads policies, roles and
 * tokens once; the overview and the list pages (which have no route of
 * their own) inherit this model. The record arrays are live, so records
 * created, edited or deleted on child pages show up without reloading.
 */
export default class AccessControlRoute extends Route {
  @service can;
  @service store;
  @service router;

  get canListAnything() {
    return (
      this.can.can('list policies') ||
      this.can.can('list roles') ||
      this.can.can('list tokens')
    );
  }

  beforeModel() {
    if (!this.canListAnything) {
      this.router.transitionTo('/jobs');
    }
  }

  model() {
    return hash({
      policies: this.can.can('list policies')
        ? this.store.findAll('policy', { reload: true })
        : [],
      roles: this.can.can('list roles')
        ? this.store.findAll('role', { reload: true })
        : [],
      tokens: this.can.can('list tokens')
        ? this.store.findAll('token', { reload: true })
        : [],
    }).catch(notifyError(this));
  }
}
