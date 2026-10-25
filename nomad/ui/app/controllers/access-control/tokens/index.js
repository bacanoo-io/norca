/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { dropTask } from 'ember-concurrency';
import LocalList from 'nomad-ui/utils/local-list';
import {
  actionsColumn,
  visibleColumns,
} from 'nomad-ui/utils/access-control/columns';

const TOKEN_TYPES = [
  { key: 'client', label: 'Client' },
  { key: 'management', label: 'Management' },
];

/**
 * The token list. Rows come from the section model; the delete button
 * goes through the acl-records service.
 */
export default class extends Controller {
  @service aclRecords;
  @service can;
  @service('token') session;

  typeOptions = TOKEN_TYPES;

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'accessor'],
    filters: {
      type: (token, chosen) => chosen.includes(token.type),
    },
  });

  get tokens() {
    return this.aclRecords.saved(this.model.tokens);
  }

  get rows() {
    return this.list.rows(this.tokens);
  }

  // The token the UI runs with; its row cannot be deleted.
  get selfTokenId() {
    return this.session.selfToken?.id;
  }

  get columns() {
    return visibleColumns(this.can, [
      { key: 'name', label: 'Name', sortable: true, hideable: false },
      { key: 'type', label: 'Type', sortable: true },
      { key: 'createTime', label: 'Created', sortable: true },
      { key: 'expirationTime', label: 'Expires', sortable: true },
      { key: 'roles', label: 'Roles' },
      { key: 'policies', label: 'Policies' },
      actionsColumn('token'),
    ]);
  }

  @dropTask *deleteToken(token) {
    yield this.aclRecords.remove('token', token);
  }
}
