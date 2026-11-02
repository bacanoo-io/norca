/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import EventStreamConnection, {
  buildStreamURL,
  NDJSONParser,
  STATES,
} from 'nomad-ui/utils/event-stream-connection';

// A fake transport: each call returns the next scripted response. A body is
// a list of NDJSON chunks.
function transport(responses) {
  const calls = [];
  const request = (url, init) => {
    calls.push(url);
    const next = responses.shift();
    if (!next) return new Promise(() => {}); // hang: the test is over
    if (next instanceof Error) return Promise.reject(next);
    const { status = 200, chunks = [] } = next;
    const encoder = new TextEncoder();
    let i = 0;
    return Promise.resolve({
      status,
      ok: status >= 200 && status < 300,
      body: {
        getReader: () => ({
          read: () =>
            init.signal.aborted || i >= chunks.length
              ? Promise.resolve({ done: true })
              : Promise.resolve({
                  value: encoder.encode(chunks[i++]),
                  done: false,
                }),
        }),
      },
    });
  };
  return { request, calls };
}

// Runs scheduled reconnects immediately, up to a limit.
function immediateScheduler(limit = 5) {
  let count = 0;
  return (fn) => {
    if (count++ < limit) Promise.resolve().then(fn);
    return count;
  };
}

const flush = () => new Promise((resolve) => setTimeout(resolve, 10));

module('Unit | Util | event-stream-connection', function () {
  test('buildStreamURL encodes topics, index, exact, region and namespace', function (assert) {
    assert.strictEqual(
      buildStreamURL({ topics: ['Job', 'Deployment:web'], index: 0 }),
      '/v1/event/stream?topic=Job&topic=Deployment%3Aweb'
    );
    assert.strictEqual(
      buildStreamURL({
        topics: ['Job'],
        index: 12,
        exact: true,
        region: 'eu',
        namespace: '*',
      }),
      '/v1/event/stream?topic=Job&index=12&exact=true&region=eu&namespace=*'
    );
    assert.strictEqual(
      buildStreamURL({ topics: ['Job'], index: 0, exact: true }),
      '/v1/event/stream?topic=Job',
      'exact is only sent with a positive index'
    );
  });

  test('NDJSONParser splits chunks into objects and keeps partial lines', function (assert) {
    const parser = new NDJSONParser();
    assert.deepEqual(parser.push('{"Index":1,"Ev'), []);
    assert.deepEqual(parser.push('ents":[]}\n{}\n{"Ind'), [
      { Index: 1, Events: [] },
      {},
    ]);
    assert.deepEqual(parser.push('ex":2,"Events":[]}\nnot json\n'), [
      { Index: 2, Events: [] },
    ]);
  });

  test('it delivers batches, skips heartbeats and resumes exactly after the last index', async function (assert) {
    const { request, calls } = transport([
      {
        chunks: [
          '{"Index":5,"Events":[{"Topic":"Job","Type":"JobRegistered","Key":"a"}]}\n',
          '{}\n',
          '{"Index":7,"Events":[{"Topic":"Job","Type":"JobRegistered","Key":"b"}]}\n',
        ],
      },
      { chunks: [] },
    ]);
    const batches = [];
    const connection = new EventStreamConnection({
      topics: ['Job'],
      region: 'eu',
      request,
      onBatch: (b) => batches.push(b.Index),
      schedule: immediateScheduler(1),
    });
    connection.start();
    await flush();
    connection.stop();

    assert.deepEqual(
      batches,
      [5, 7],
      'both batches delivered, heartbeat ignored'
    );
    assert.strictEqual(
      calls[0],
      '/v1/event/stream?topic=Job&region=eu',
      'first connect starts at the head'
    );
    assert.strictEqual(
      calls[1],
      '/v1/event/stream?topic=Job&index=8&exact=true&region=eu',
      'reconnect resumes after the last delivered index with exact=true'
    );
  });

  test('replay starts from the buffered events without exact', async function (assert) {
    const { request, calls } = transport([{ chunks: [] }]);
    const connection = new EventStreamConnection({
      topics: ['Node'],
      request,
      replay: true,
      onBatch() {},
      schedule: immediateScheduler(0),
    });
    connection.start();
    await flush();
    connection.stop();
    assert.strictEqual(calls[0], '/v1/event/stream?topic=Node&index=1');
  });

  test('a 410 Gone triggers a resync and continues after the refetched index', async function (assert) {
    const { request, calls } = transport([
      { chunks: ['{"Index":3,"Events":[{"Topic":"Job"}]}\n'] },
      { status: 410 },
      { chunks: [] },
    ]);
    let resyncs = 0;
    const connection = new EventStreamConnection({
      topics: ['Job'],
      request,
      onBatch() {},
      onResync: () => {
        resyncs++;
        return Promise.resolve(42);
      },
      schedule: immediateScheduler(3),
    });
    connection.start();
    await flush();
    connection.stop();

    assert.strictEqual(resyncs, 1, 'subscribers were asked to refetch once');
    assert.strictEqual(
      calls[1],
      '/v1/event/stream?topic=Job&index=4&exact=true'
    );
    assert.strictEqual(
      calls[2],
      '/v1/event/stream?topic=Job&index=43&exact=true',
      'the stream continues right after the index of the refetched state'
    );
  });

  test('a 403 stops the connection in the forbidden state', async function (assert) {
    const { request, calls } = transport([{ status: 403 }]);
    const states = [];
    const connection = new EventStreamConnection({
      topics: ['Node'],
      request,
      onBatch() {},
      onStateChange: (s) => states.push(s),
      schedule: immediateScheduler(3),
    });
    connection.start();
    await flush();
    assert.strictEqual(calls.length, 1, 'no retry');
    assert.strictEqual(connection.state, STATES.forbidden);
    assert.deepEqual(states, [STATES.connecting, STATES.forbidden]);
    connection.stop();
  });

  test('network errors are retried with backoff', async function (assert) {
    const delays = [];
    const { request, calls } = transport([
      new Error('offline'),
      new Error('offline'),
      { chunks: [] },
    ]);
    const connection = new EventStreamConnection({
      topics: ['Job'],
      request,
      onBatch() {},
      schedule: (fn, ms) => {
        delays.push(ms);
        if (delays.length < 3) Promise.resolve().then(fn);
        return delays.length;
      },
    });
    connection.start();
    await flush();
    connection.stop();
    assert.ok(calls.length >= 3, 'reconnected after the failures');
    assert.ok(delays[1] > delays[0], 'the delay grows');
  });

  test('stop aborts the request and prevents reconnects', async function (assert) {
    let signal;
    const connection = new EventStreamConnection({
      topics: ['Job'],
      request: (url, init) => {
        signal = init.signal;
        return new Promise(() => {});
      },
      onBatch() {},
    });
    connection.start();
    await flush();
    connection.stop();
    assert.true(signal.aborted);
    assert.strictEqual(connection.state, STATES.closed);
  });
});
