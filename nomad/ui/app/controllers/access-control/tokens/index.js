/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';
import LocalList from 'nomad-ui/utils/local-list';

export default class AccessControlTokensIndexController extends Controller {
  @service can;
  @service notifications;
  @service('token') tokenService;

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'accessor'],
    filters: {
      type: (token, selection) => selection.includes(token.type),
    },
  });

  typeOptions = [
    { key: 'client', label: 'Client' },
    { key: 'management', label: 'Management' },
  ];

  /**
   * Saved tokens; drafts from the "new token" page are left out.
   */
  get tokens() {
    return (this.model.tokens || []).filter(
      (token) => !token.isNew && !token.isDeleted
    );
  }

  get rows() {
    return this.list.rows(this.tokens);
  }

  get selfTokenId() {
    return this.tokenService.selfToken?.id;
  }

  get columns() {
    const columns = [
      { key: 'name', label: 'Name', sortable: true, hideable: false },
      { key: 'type', label: 'Type', sortable: true },
      { key: 'createTime', label: 'Created', sortable: true },
      { key: 'expirationTime', label: 'Expires', sortable: true },
      { key: 'roles', label: 'Roles' },
      { key: 'policies', label: 'Policies' },
    ];
    if (this.can.can('destroy token')) {
      columns.push({
        key: 'actions',
        label: 'Actions',
        srOnly: true,
        hideable: false,
      });
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
