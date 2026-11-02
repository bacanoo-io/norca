/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { dropTask } from 'ember-concurrency';
import LocalList from 'nomad-ui/utils/local-list';
import {
  actionsColumn,
  visibleColumns,
} from 'nomad-ui/utils/access-control/columns';

/**
 * The role list, with the number of tokens that carry each role.
 */
export default class extends Controller {
  @service aclRecords;
  @service can;

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'description'],
  });

  get roles() {
    return this.aclRecords.saved(this.model.roles);
  }

  get rows() {
    return this.list.rows(this.roles);
  }

  // role ID -> number of saved tokens linked to it
  get tokenCounts() {
    return this.aclRecords.tokenLinks(this.model.tokens, 'roles');
  }

  get columns() {
    return visibleColumns(this.can, [
      { key: 'name', label: 'Role Name', sortable: true, hideable: false },
      { key: 'description', label: 'Description' },
      { key: 'policies', label: 'Policies' },
      { key: 'tokens', label: 'Tokens', requires: 'list tokens' },
      actionsColumn('role'),
    ]);
  }

  @action openRole(role, event) {
    this.aclRecords.open('role', role, event);
  }

  @dropTask *deleteRole(role) {
    yield this.aclRecords.remove('role', role);
  }
}
