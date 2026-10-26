/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupTest } from 'ember-qunit';
import { matchesTopics } from 'nomad-ui/services/event-stream';

const flush = () => new Promise((resolve) => setTimeout(resolve, 10));

// Serves one scripted NDJSON body per request and records the URLs. With
// `only`, requests whose URL does not contain it stay pending (connections
// that are replaced before they are read).
function stubRequests(service, bodies, only) {
  const calls = [];
  service.request = (url, init) => {
    calls.push(url);
    if (only && !url.includes(only)) return new Promise(() => {});
    const body = bodies.shift();
    if (body === undefined) return new Promise(() => {});
    return Promise.resolve({
      status: 200,
      ok: true,
      body: null,
      text: () =>
        init.signal.aborted ? Promise.resolve('') : Promise.resolve(body),
    });
  };
  return calls;
}

module('Unit | Service | event-stream', function (hooks) {
  setupTest(hooks);

  hooks.beforeEach(function () {
    this.service = this.owner.lookup('service:event-stream');
    this.service.enabled = true;
  });

  hooks.afterEach(function () {
    for (const { connection } of this.service.connections.values())
      connection.stop();
  });

  test('matchesTopics understands topics, keys and wildcards', function (assert) {
    const event = { Topic: 'Job', Key: 'web' };
    assert.true(matchesTopics(event, ['Job']));
    assert.true(matchesTopics(event, ['Job:web']));
    assert.false(matchesTopics(event, ['Job:api']));
    assert.false(matchesTopics(event, ['Node']));
    assert.true(matchesTopics(event, ['*']));
  });

  test('subscribers of a region share one connection with the union of their topics', async function (assert) {
    const calls = stubRequests(this.service, []);
    const off1 = this.service.subscribe({ topics: ['Job'], region: 'eu' });
    const off2 = this.service.subscribe({
      topics: ['Node', 'Job'],
      region: 'eu',
    });
    await flush();

    assert.strictEqual(this.service.connections.size, 1);
    assert.ok(
      calls[calls.length - 1].includes('topic=Job&topic=Node'),
      'the connection subscribes to the union'
    );

    off2();
    off1();
    await flush();
    assert.strictEqual(
      this.service.connections.size,
      0,
      'the last unsubscribe closes it'
    );
    assert.strictEqual(this.service.stateFor('eu'), 'closed');
  });

  test('events are dispatched to the matching subscribers and kept as recent events', async function (assert) {
    stubRequests(
      this.service,
      [
        '{"Index":10,"Events":[{"Topic":"Job","Key":"web","Type":"JobRegistered"},{"Topic":"Node","Key":"n1","Type":"NodeRegistration"}]}\n',
      ],
      'topic=Job&topic=Node'
    );
    const jobEvents = [];
    const nodeEvents = [];
    this.service.subscribe({
      topics: ['Job'],
      region: 'eu',
      onEvents: (e) => jobEvents.push(...e),
    });
    this.service.subscribe({
      topics: ['Node'],
      region: 'eu',
      onEvents: (e) => nodeEvents.push(...e),
    });
    await flush();

    assert.deepEqual(
      jobEvents.map((e) => e.Key),
      ['web']
    );
    assert.deepEqual(
      nodeEvents.map((e) => e.Key),
      ['n1']
    );
    assert.strictEqual(
      jobEvents[0].Region,
      'eu',
      'events are tagged with their region'
    );
    assert.deepEqual(
      this.service.recentEvents.map((e) => e.Key),
      ['n1', 'web'],
      'recent events, newest first'
    );
  });

  test('resync asks every subscriber to refetch and continues after the oldest state', async function (assert) {
    this.service.enabled = false; // no connection needed for this one
    this.service.subscribe({
      topics: ['Job'],
      region: 'eu',
      onResync: () => 30,
    });
    this.service.subscribe({
      topics: ['Node'],
      region: 'eu',
      onResync: () => Promise.resolve(20),
    });
    this.service.subscribe({
      topics: ['Node'],
      region: 'us',
      onResync: () => 5,
    });

    const index = await this.service.resync('eu|*');
    assert.strictEqual(index, 20);
  });

  test('it does nothing while disabled', async function (assert) {
    this.service.enabled = false;
    const calls = stubRequests(this.service, []);
    this.service.subscribe({ topics: ['Job'] });
    await flush();
    assert.strictEqual(calls.length, 0);
  });
});
