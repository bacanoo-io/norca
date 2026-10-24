/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { alias } from '@ember/object/computed';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';

export default class AccessControlTokensTokenController extends Controller {
  @service notifications;
  @service router;
  @service('token') tokenService;

  @alias('model.token') tokenRecord;
  @alias('model.policies') policies;
  @alias('model.roles') roles;

  /**
   * The token the UI is signed in with cannot delete itself.
   */
  get isOwnToken() {
    return this.tokenRecord?.id === this.tokenService.selfToken?.id;
  }

  @task(function* () {
    const name = this.tokenRecord.name;
    try {
      yield this.tokenRecord.destroyRecord();
      this.notifications.add({
        title: `Token ${name} deleted`,
        color: 'success',
      });
      this.router.transitionTo('access-control.tokens');
    } catch (err) {
      this.tokenRecord.rollbackAttributes();
      this.notifications.add({
        title: `Error deleting token ${name}`,
        message: messageFromAdapterError(err, 'delete tokens'),
        color: 'critical',
        sticky: true,
      });
    }
  })
  deleteToken;
}
