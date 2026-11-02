/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { dropTask } from 'ember-concurrency';
import { visibleColumns } from 'nomad-ui/utils/access-control/columns';

/**
 * The page of one role: the role-editor, and the tokens that carry the
 * role with a hint how to create another one from the CLI.
 */
export default class extends Controller {
  @service aclRecords;
  @service can;
  @service router;

  get role() {
    return this.model.role;
  }

  get policies() {
    return this.model.policies;
  }

  get tokens() {
    const id = this.role.id;
    return this.aclRecords
      .saved(this.model.tokens)
      .filter((token) => token.hasMany('roles').ids().includes(id));
  }

  get tokenColumns() {
    return visibleColumns(this.can, [
      { key: 'name', label: 'Name' },
      { key: 'created', label: 'Created' },
      { key: 'expires', label: 'Expires' },
      {
        key: 'actions',
        label: 'Actions',
        srOnly: true,
        requires: 'destroy token',
      },
    ]);
  }

  get newTokenString() {
    return [
      'norca acl token create',
      '-name="<TOKEN_NAME>"',
      `-role-name="${this.role.name}"`,
      '-type=client',
      '-ttl=8h',
    ].join(' ');
  }

  @dropTask *deleteRole() {
    if (yield this.aclRecords.remove('role', this.role)) {
      this.router.transitionTo('access-control.roles');
    }
  }

  @dropTask *deleteToken(token) {
    yield this.aclRecords.remove('token', token);
  }
}
