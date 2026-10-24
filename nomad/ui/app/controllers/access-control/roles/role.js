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

export default class AccessControlRolesRoleController extends Controller {
  @service notifications;
  @service router;
  @service store;

  @alias('model.role') role;
  @alias('model.policies') policies;

  /**
   * Saved tokens that are linked to this role.
   */
  get tokens() {
    return (this.model.tokens || []).filter(
      (token) =>
        !token.isNew &&
        !token.isDeleted &&
        token.hasMany('roles').ids().includes(this.role.id)
    );
  }

  get newTokenString() {
    return `nomad acl token create -name="<TOKEN_NAME>" -role-name="${this.role.name}" -type=client -ttl=8h`;
  }

  @task(function* () {
    const role = this.role;
    const name = role.name;
    try {
      yield role.destroyRecord();

      // Drop references to the deleted role from loaded tokens
      this.store.peekAll('token').forEach((token) => {
        token.roles.removeObject(role);
      });

      this.notifications.add({
        title: `Role ${name} deleted`,
        color: 'success',
      });
      this.router.transitionTo('access-control.roles');
    } catch (err) {
      role.rollbackAttributes();
      this.notifications.add({
        title: `Error deleting role ${name}`,
        message: messageFromAdapterError(err, 'delete roles'),
        color: 'critical',
        sticky: true,
      });
    }
  })
  deleteRole;

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
