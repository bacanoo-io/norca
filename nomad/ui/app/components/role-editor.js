/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { tracked } from '@glimmer/tracking';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';

// Same rules as the ACL role API applies
const NAME_PATTERN = /^[a-zA-Z0-9-]{1,128}$/;
const MAX_DESCRIPTION_LENGTH = 256;

/**
 * Form to create or edit an ACL role: name, description and the policies
 * the role grants.
 *
 * @param {Role} role - the role record to edit (may be new)
 * @param {Policy[]} policies - policies that can be linked
 */
export default class RoleEditorComponent extends Component {
  @service notifications;
  @service router;
  @service store;

  @tracked selectedPolicyIds = [];

  policyColumns = [
    { key: 'selected', label: 'Linked' },
    { key: 'name', label: 'Policy', isSortable: true },
    { key: 'description', label: 'Description' },
  ];

  constructor() {
    super(...arguments);
    this.resetSelection();
  }

  get role() {
    return this.args.role;
  }

  get availablePolicies() {
    return (this.args.policies || []).filter(
      (policy) => !policy.isNew && !policy.isDeleted
    );
  }

  resetSelection() {
    this.selectedPolicyIds = this.role.hasMany('policies').ids();
  }

  @action updateName({ target: { value } }) {
    this.role.set('name', value);
  }

  @action updateDescription({ target: { value } }) {
    this.role.set('description', value);
  }

  @action togglePolicy(policy, { target: { checked } }) {
    const without = this.selectedPolicyIds.filter((id) => id !== policy.id);
    this.selectedPolicyIds = checked ? [...without, policy.id] : without;
  }

  validate() {
    const name = this.role.name || '';
    if (!NAME_PATTERN.test(name)) {
      throw new Error(
        'Role name must be 1-128 characters long and can only contain letters, numbers, and dashes.'
      );
    }
    const nameTaken = this.store
      .peekAll('role')
      .any(
        (role) =>
          role !== this.role &&
          !role.isNew &&
          !role.isDeleted &&
          role.name === name
      );
    if (nameTaken) {
      throw new Error(`A role with name ${name} already exists.`);
    }
    if ((this.role.description || '').length > MAX_DESCRIPTION_LENGTH) {
      throw new Error(
        `Description must be at most ${MAX_DESCRIPTION_LENGTH} characters long.`
      );
    }
    if (!this.selectedPolicyIds.length) {
      throw new Error('A role must have at least one policy.');
    }
  }

  recordsFor(ids) {
    return ids.map((id) => this.store.peekRecord('policy', id)).filter(Boolean);
  }

  @task(function* (event) {
    if (event instanceof Event) {
      event.preventDefault();
    }

    const role = this.role;
    const isNew = role.isNew;
    let previousPolicies = null;

    try {
      this.validate();
      previousPolicies = this.recordsFor(role.hasMany('policies').ids());
      role.set('policies', this.recordsFor(this.selectedPolicyIds));
      yield role.save();

      this.notifications.add({
        title: isNew ? 'Role created' : 'Role saved',
        color: 'success',
      });

      if (isNew) {
        this.router.transitionTo('access-control.roles.role', role.id);
      } else {
        this.resetSelection();
      }
    } catch (err) {
      if (previousPolicies && !role.isNew) {
        role.set('policies', previousPolicies);
      }
      this.notifications.add({
        title: `Error saving role ${role.name || ''}`.trim(),
        message:
          err instanceof Error && !err.errors
            ? err.message
            : messageFromAdapterError(err, 'save roles'),
        color: 'critical',
        sticky: true,
      });
    }
  })
  save;
}
