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

const MAX_NAME_LENGTH = 256;

/**
 * Expiration choices offered when creating a token. Values other than
 * "never" and "custom" are sent as the token's ExpirationTTL.
 */
export const EXPIRATION_OPTIONS = [
  { value: 'never', label: 'Never' },
  { value: '10m', label: '10 minutes' },
  { value: '8h', label: '8 hours' },
  { value: '24h', label: '24 hours' },
  { value: 'custom', label: 'Custom' },
];

/**
 * Form to create or edit an ACL token: name, type, global flag, expiration
 * (on creation only) and, for client tokens, the linked roles and policies.
 *
 * @param {Token} token - the token record to edit (may be new)
 * @param {Policy[]} policies - policies that can be linked
 * @param {Role[]} roles - roles that can be linked
 */
export default class TokenEditorComponent extends Component {
  @service notifications;
  @service router;
  @service store;

  expirationOptions = EXPIRATION_OPTIONS;

  @tracked expiration = 'never';
  @tracked customExpiration = '';
  @tracked selectedPolicyIds = [];
  @tracked selectedRoleIds = [];

  constructor() {
    super(...arguments);
    this.resetSelection();
  }

  get tokenRecord() {
    return this.args.token;
  }

  get isClientToken() {
    return this.tokenRecord.type !== 'management';
  }

  get availablePolicies() {
    return (this.args.policies || []).filter(
      (policy) => !policy.isNew && !policy.isDeleted
    );
  }

  get availableRoles() {
    return (this.args.roles || []).filter(
      (role) => !role.isNew && !role.isDeleted
    );
  }

  policyColumns = [
    { key: 'selected', label: 'Linked' },
    { key: 'name', label: 'Policy', isSortable: true },
    { key: 'description', label: 'Description' },
  ];

  roleColumns = [
    { key: 'selected', label: 'Linked' },
    { key: 'name', label: 'Role', isSortable: true },
    { key: 'description', label: 'Description' },
    { key: 'policies', label: 'Policies' },
  ];

  /**
   * Minimum value for the custom expiration picker (now, local time).
   */
  get minExpiration() {
    return toLocalInputValue(new Date());
  }

  resetSelection() {
    this.selectedPolicyIds = this.tokenRecord.hasMany('policies').ids();
    this.selectedRoleIds = this.tokenRecord.hasMany('roles').ids();
  }

  @action updateName(value) {
    this.tokenRecord.set('name', value);
  }

  @action updateType(type) {
    this.tokenRecord.set('type', type);
  }

  @action toggleGlobal(checked) {
    this.tokenRecord.set('global', checked);
  }

  @action setExpiration(value) {
    this.expiration = value;
  }

  // Ui::Input passes the value, a native input the event.
  @action updateCustomExpiration(input) {
    const value = typeof input === 'string' ? input : input?.target?.value;
    this.customExpiration = value;
  }

  @action togglePolicy(policy, checked) {
    this.selectedPolicyIds = toggle(this.selectedPolicyIds, policy.id, checked);
  }

  @action toggleRole(role, checked) {
    this.selectedRoleIds = toggle(this.selectedRoleIds, role.id, checked);
  }

  validate() {
    const name = this.tokenRecord.name || '';
    if (name.length > MAX_NAME_LENGTH) {
      throw new Error(
        `Token name must be at most ${MAX_NAME_LENGTH} characters long.`
      );
    }
    if (this.tokenRecord.isNew && this.expiration === 'custom') {
      const time = new Date(this.customExpiration);
      if (!this.customExpiration || isNaN(time.getTime())) {
        throw new Error('Choose an expiration date and time.');
      }
      if (time <= new Date()) {
        throw new Error('The expiration time must be in the future.');
      }
    }
  }

  /**
   * Copies the form state that isn't bound directly (links and expiration)
   * onto the record, returning what to restore if saving fails.
   */
  applyToRecord() {
    const token = this.tokenRecord;
    const previous = {
      policies: this.recordsFor('policy', token.hasMany('policies').ids()),
      roles: this.recordsFor('role', token.hasMany('roles').ids()),
    };

    // Management tokens have every permission and may not link policies or roles
    const policies = this.isClientToken
      ? this.recordsFor('policy', this.selectedPolicyIds)
      : [];
    const roles = this.isClientToken
      ? this.recordsFor('role', this.selectedRoleIds)
      : [];
    token.setProperties({ policies, roles });

    if (token.isNew) {
      if (this.expiration === 'custom') {
        token.setProperties({
          expirationTime: new Date(this.customExpiration),
          expirationTTL: null,
        });
      } else if (this.expiration === 'never') {
        token.setProperties({ expirationTime: null, expirationTTL: null });
      } else {
        token.setProperties({
          expirationTime: null,
          expirationTTL: this.expiration,
        });
      }
    }

    return previous;
  }

  recordsFor(modelName, ids) {
    return ids
      .map((id) => this.store.peekRecord(modelName, id))
      .filter(Boolean);
  }

  @task(function* (event) {
    if (event instanceof Event) {
      event.preventDefault();
    }

    const token = this.tokenRecord;
    const isNew = token.isNew;
    let previous = null;

    try {
      this.validate();
      previous = this.applyToRecord();
      yield token.save();

      this.notifications.add({
        title: isNew ? 'Token created' : 'Token saved',
        color: 'success',
      });

      if (isNew) {
        this.router.transitionTo('access-control.tokens.token', token.id);
      } else {
        this.resetSelection();
      }
    } catch (err) {
      if (previous && !token.isNew) {
        token.setProperties(previous);
      }
      this.notifications.add({
        title: `Error saving token ${token.name || ''}`.trim(),
        message:
          err instanceof Error && !err.errors
            ? err.message
            : messageFromAdapterError(err, 'save tokens'),
        color: 'critical',
        sticky: true,
      });
    }
  })
  save;
}

function toggle(list, id, include) {
  const without = list.filter((item) => item !== id);
  return include ? [...without, id] : without;
}

function toLocalInputValue(date) {
  const offset = date.getTimezoneOffset() * 60000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 16);
}
