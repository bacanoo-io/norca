/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupTest } from 'ember-qunit';

import { run } from '@ember/runloop';

module('Unit | Model | reschedule-event', function (hooks) {
  setupTest(hooks);

  test('the short IDs come from the previous allocation and node IDs', function (assert) {
    const allocation = run(() =>
      this.owner.lookup('service:store').createRecord('allocation', {
        rescheduleEvents: [
          {
            previousAllocationId: 'aaaaaaaa-1111-2222-3333-444444444444',
            previousNodeId: 'bbbbbbbb-1111-2222-3333-444444444444',
          },
        ],
      })
    );
    const event = allocation.rescheduleEvents.objectAt(0);

    assert.strictEqual(event.previousAllocationShortId, 'aaaaaaaa');
    assert.strictEqual(event.previousNodeShortId, 'bbbbbbbb');
  });
});
