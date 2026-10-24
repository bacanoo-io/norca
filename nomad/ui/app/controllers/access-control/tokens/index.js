/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';

export default class AccessControlTokensIndexController extends Controller {
  @service can;
  @service notifications;
  @service('token') tokenService;

  /**
   * Saved tokens; drafts from the "new token" page are left out.
   */
  get tokens() {
    return (this.model.tokens || []).filter(
      (token) => !token.isNew && !token.isDeleted
    );
  }

  get selfTokenId() {
    return this.tokenService.selfToken?.id;
  }

  get columns() {
    const columns = [
      { key: 'name', label: 'Name', isSortable: true },
      { key: 'type', label: 'Type', isSortable: true },
      { key: 'createTime', label: 'Created', isSortable: true },
      { key: 'expirationTime', label: 'Expires', isSortable: true },
      { key: 'roles', label: 'Roles' },
      { key: 'policies', label: 'Policies' },
    ];
    if (this.can.can('destroy token')) {
      columns.push({ key: 'delete', label: 'Delete' });
    }
    return columns;
  }

  @task(function* (token) {
    const name = token.name;
    try {
      yield token.destroyRecord();
      this.notifications.add({
        title: `Token ${name} deleted`,
        color: 'success',
      });
    } catch (err) {
      token.rollbackAttributes();
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
