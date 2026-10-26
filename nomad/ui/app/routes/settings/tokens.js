/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { A } from '@ember/array';

export default class SettingsTokensRoute extends Route {
  @service store;
  // Without ACLs the auth methods endpoint answers 400: no SSO methods then.
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
