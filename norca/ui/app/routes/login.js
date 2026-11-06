/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { A } from '@ember/array';

/**
 * The sign-in page of a cluster with ACLs. A visitor without a valid token
 * is sent here from every other page (routes/application); one with a token,
 * or a cluster without ACLs, goes on to the dashboard.
 */
export default class LoginRoute extends Route {
  @service store;
  @service token;
  @service router;

  beforeModel() {
    if (this.token.checked && !this.token.needsSignIn) {
      this.router.transitionTo('index');
    }
  }

  // The SSO sign-in methods (none without ACLs).
  async model() {
    let authMethods;
    try {
      authMethods = await this.store.findAll('auth-method');
    } catch (e) {
      authMethods = A([]);
    }
    return { authMethods };
  }
}
