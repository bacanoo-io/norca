/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember/avoid-leaking-state-in-ember-objects */
import { module, test } from 'qunit';
import { setupTest } from 'ember-qunit';
import Service from '@ember/service';
import setupAbility from 'nomad-ui/tests/helpers/setup-ability';

const ACTIONS = ['canRead', 'canList', 'canWrite', 'canUpdate', 'canDestroy'];

module('Unit | Ability | role', function (hooks) {
  setupTest(hooks);
  setupAbility('role')(hooks);

  test('a client token cannot manage roles', function (assert) {
    assert.expect(ACTIONS.length);
    this.owner.register(
      'service:token',
      Service.extend({ aclEnabled: true, selfToken: { type: 'client' } })
    );
    ACTIONS.forEach((name) => assert.notOk(this.ability[name], name));
  });

  test('a management token can manage roles', function (assert) {
    assert.expect(ACTIONS.length);
    this.owner.register(
      'service:token',
      Service.extend({ aclEnabled: true, selfToken: { type: 'management' } })
    );
    ACTIONS.forEach((name) => assert.ok(this.ability[name], name));
  });

  test('without ACLs there is nothing to manage', function (assert) {
    assert.expect(ACTIONS.length);
    this.owner.register('service:token', Service.extend({ aclEnabled: false }));
    ACTIONS.forEach((name) => assert.notOk(this.ability[name], name));
  });
});
