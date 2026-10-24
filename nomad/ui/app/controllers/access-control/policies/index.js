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

export default class AccessControlPoliciesIndexController extends Controller {
  @service can;
  @service notifications;
  @service router;
  @service store;

  /**
   * Saved policies; the draft from the "new policy" page is left out.
   */
  get policies() {
    return (this.model.policies || []).filter(
      (policy) => !policy.isNew && !policy.isDeleted
    );
  }

  /**
   * Number of tokens (and expired tokens) that reference each policy
   * directly, keyed by policy name.
   */
  get tokenCounts() {
    const counts = {};
    (this.model.tokens || []).forEach((token) => {
      if (token.isNew || token.isDeleted) return;
      token
        .hasMany('policies')
        .ids()
        .forEach((name) => {
          counts[name] = counts[name] || { total: 0, expired: 0 };
          counts[name].total++;
          if (token.isExpired) {
            counts[name].expired++;
          }
        });
    });
    return counts;
  }

  get columns() {
    const columns = [
      { key: 'name', label: 'Policy Name', isSortable: true },
      { key: 'description', label: 'Description' },
    ];
    if (this.can.can('list tokens')) {
      columns.push({ key: 'tokens', label: 'Tokens' });
    }
    if (this.can.can('destroy policy')) {
      columns.push({ key: 'delete', label: 'Delete' });
    }
    return columns;
  }

  /**
   * Opens a policy when its row is clicked, unless the click was on a
   * control inside the row (the name link or the delete button).
   */
  @action openPolicy(policy, event) {
    if (event?.target?.closest?.('a, button, input')) {
      return;
    }
    this.router.transitionTo('access-control.policies.policy', policy.name);
  }

  @task(function* (policy) {
    const name = policy.name;
    try {
      yield policy.destroyRecord();

      // Drop references to the deleted policy from loaded roles and tokens
      // so lists don't keep showing it
      this.store.peekAll('role').forEach((role) => {
        role.policies.removeObject(policy);
      });
      this.store.peekAll('token').forEach((token) => {
        token.policies.removeObject(policy);
      });

      this.notifications.add({
        title: `Policy ${name} deleted`,
        color: 'success',
      });
    } catch (err) {
      policy.rollbackAttributes();
      this.notifications.add({
        title: `Error deleting policy ${name}`,
        message: messageFromAdapterError(err, 'delete policies'),
        color: 'critical',
        sticky: true,
      });
    }
  })
  deletePolicy;
}
