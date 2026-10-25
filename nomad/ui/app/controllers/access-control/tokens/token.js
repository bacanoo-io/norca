/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { dropTask } from 'ember-concurrency';

/**
 * The page of one token, edited with the token-editor component.
 */
export default class extends Controller {
  @service aclRecords;
  @service router;
  @service('token') session;

  get tokenRecord() {
    return this.model.token;
  }

  get policies() {
    return this.model.policies;
  }

  get roles() {
    return this.model.roles;
  }

  // A token cannot delete itself: the UI would lose its own access.
  get isOwnToken() {
    const self = this.session.selfToken;
    return Boolean(self) && self.id === this.tokenRecord?.id;
  }

  @dropTask *deleteToken() {
    if (yield this.aclRecords.remove('token', this.tokenRecord)) {
      this.router.transitionTo('access-control.tokens');
    }
  }
}
