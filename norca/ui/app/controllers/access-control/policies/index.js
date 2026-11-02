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
 * The policy list, with the number of tokens (and how many of them have
 * expired) that name each policy directly.
 */
export default class extends Controller {
  @service aclRecords;
  @service can;

  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name', 'description'],
  });

  get policies() {
    return this.aclRecords.saved(this.model.policies);
  }

  get rows() {
    return this.list.rows(this.policies);
  }

  // policy name -> { total, expired } over the saved tokens
  get tokenCounts() {
    return this.aclRecords.tokenLinks(this.model.tokens, 'policies', {
      expired: true,
    });
  }

  get columns() {
    return visibleColumns(this.can, [
      { key: 'name', label: 'Policy Name', sortable: true, hideable: false },
      { key: 'description', label: 'Description' },
      { key: 'tokens', label: 'Tokens', requires: 'list tokens' },
      actionsColumn('policy'),
    ]);
  }

  @action openPolicy(policy, event) {
    this.aclRecords.open('policy', policy, event);
  }

  @dropTask *deletePolicy(policy) {
    yield this.aclRecords.remove('policy', policy);
  }
}
