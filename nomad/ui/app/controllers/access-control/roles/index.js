/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Controller from '@ember/controller';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';

export default class AccessControlRolesIndexController extends Controller {
  @service can;
  @service notifications;
  @service router;
  @service store;

  /**
   * Saved roles; the draft from the "new role" page is left out.
   */
  get roles() {
    return (this.model.roles || []).filter(
      (role) => !role.isNew && !role.isDeleted
    );
  }

  /**
   * Number of tokens linked to each role, keyed by role ID.
   */
  get tokenCounts() {
    const counts = {};
    (this.model.tokens || []).forEach((token) => {
      if (token.isNew || token.isDeleted) return;
      token
        .hasMany('roles')
        .ids()
        .forEach((id) => {
          counts[id] = (counts[id] || 0) + 1;
        });
    });
    return counts;
  }

  get columns() {
    const columns = [
      { key: 'name', label: 'Role Name', isSortable: true },
      { key: 'description', label: 'Description' },
      { key: 'policies', label: 'Policies' },
    ];
    if (this.can.can('list tokens')) {
      columns.push({ key: 'tokens', label: 'Tokens' });
    }
    if (this.can.can('destroy role')) {
      columns.push({ key: 'delete', label: 'Delete' });
    }
    return columns;
  }

  /**
   * Opens a role when its row is clicked, unless the click was on a control
   * inside the row (a link or the delete button).
   */
  @action openRole(role, event) {
    if (event?.target?.closest?.('a, button, input')) {
      return;
    }
    this.router.transitionTo('access-control.roles.role', role.id);
  }

  @task(function* (role) {
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
}
