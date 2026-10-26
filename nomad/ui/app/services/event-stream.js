/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service, { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import EventStreamConnection, {
  STATES,
} from 'nomad-ui/utils/event-stream-connection';

const RECENT_LIMIT = 50;

/**
 * Client of the event stream (/v1/event/stream). The UI otherwise uses
 * blocking queries; views that want live updates subscribe here.
 *
 *   const unsubscribe = this.eventStream.subscribe({
 *     topics: ['Deployment', 'Job:example'],
 *     region: 'eu-west',          // '' = the agent's region
 *     namespace: '*',             // default: '*'
 *     replay: true,               // start with the events still buffered
 *     onEvents(events, index) {}, // every batch with matching events
 *     onResync() {},              // continuity lost (410 Gone): refetch;
 *                                 // may return (a promise of) the
 *                                 // X-Nomad-Index of the refetched state
 *   });
 *
 * One connection is kept per region and namespace (UI plan: one event
 * stream connection per region updates all open views), subscribed to the
 * union of the topics of its subscribers. It resumes after the last
 * delivered index with `exact=true`; see utils/event-stream-connection.
 */
export default class EventStreamService extends Service {
  @service token;

  /** `${region}|${namespace}` -> connection state */
  @tracked states = {};

  /** the latest events of all connections, newest first */
  @tracked recentEvents = [];

  subscriptions = new Set();
  connections = new Map();

  // The test environment has no streaming mock server; tests enable the
  // service explicitly (and stub `request`).
  enabled = config.APP.eventStream !== false;

  request(url, init) {
    return this.token.authorizedRawRequest(url, init);
  }

  subscribe({
    topics,
    region = '',
    namespace = '*',
    replay = false,
    onEvents,
    onResync,
  }) {
    const subscription = {
      replay,
      topics: [...new Set(topics || [])],
      key: connectionKey(region, namespace),
      region,
      namespace,
      onEvents,
      onResync,
    };
    this.subscriptions.add(subscription);
    this.scheduleReconcile(subscription);
    return () => {
      if (!this.subscriptions.delete(subscription)) return;
      this.scheduleReconcile(subscription);
    };
  }

  // Views subscribe and unsubscribe while they render (modifiers), and the
  // connection state is tracked: the connections are adjusted right after,
  // once for all the subscriptions of the render.
  scheduleReconcile(subscription) {
    waitForPromise(
      Promise.resolve().then(() => {
        if (this.isDestroying || this.isDestroyed) return;
        this.reconcile(subscription);
      })
    );
  }

  stateFor(region = '', namespace = '*') {
    return this.states[connectionKey(region, namespace)] || STATES.idle;
  }

  setState(key, state) {
    if (this.isDestroying || this.isDestroyed) return;
    this.states = { ...this.states, [key]: state };
  }

  topicsFor(key) {
    const topics = new Set();
    for (const s of this.subscriptions) {
      if (s.key === key) s.topics.forEach((t) => topics.add(t));
    }
    return [...topics].sort();
  }

  reconcile({ key, region, namespace }) {
    if (!this.enabled) return;
    const topics = this.topicsFor(key);
    const existing = this.connections.get(key);
    const topicKey = topics.join('&');

    if (existing && existing.topicKey === topicKey) return;

    let resumeIndex = 0;
    if (existing) {
      resumeIndex = existing.connection.lastIndex;
      existing.connection.stop();
      this.connections.delete(key);
    }
    if (!topics.length) {
      this.setState(key, STATES.closed);
      return;
    }

    const replay = [...this.subscriptions].some(
      (s) => s.key === key && s.replay
    );
    const connection = new EventStreamConnection({
      topics,
      region,
      namespace,
      replay: replay && !resumeIndex,
      request: (url, init) => this.request(url, init),
      onBatch: (batch) => this.dispatch(key, region, batch),
      onResync: () => this.resync(key),
      onStateChange: (state) => this.setState(key, state),
    });
    connection.resumeFrom(resumeIndex);
    this.connections.set(key, { topicKey, connection });
    connection.start();
  }

  dispatch(key, region, { Index, Events }) {
    if (this.isDestroying || this.isDestroyed) return;
    const tagged = Events.map((event) => ({ ...event, Region: region }));
    this.recentEvents = [...tagged.reverse(), ...this.recentEvents].slice(
      0,
      RECENT_LIMIT
    );

    for (const s of this.subscriptions) {
      if (s.key !== key || !s.onEvents) continue;
      const matching = tagged.filter((e) => matchesTopics(e, s.topics));
      if (matching.length) s.onEvents(matching, Index);
    }
  }

  // Asks the subscribers of a connection to refetch; resolves to the index
  // the stream continues after: the oldest refetched state, so that no
  // subscriber misses an event between its refetch and the restart.
  async resync(key) {
    const results = [];
    for (const s of this.subscriptions) {
      if (s.key === key && s.onResync) {
        results.push(Promise.resolve().then(() => s.onResync()));
      }
    }
    const settled = await Promise.allSettled(results);
    const known = settled
      .filter((r) => r.status === 'fulfilled' && Number(r.value) > 0)
      .map((r) => Number(r.value));
    return known.length ? Math.min(...known) : 0;
  }

  willDestroy() {
    for (const { connection } of this.connections.values()) connection.stop();
    this.connections.clear();
    this.subscriptions.clear();
    super.willDestroy(...arguments);
  }
}

function connectionKey(region, namespace) {
  return `${region || ''}|${namespace || '*'}`;
}

// A subscription topic is "Topic" or "Topic:Key"; "*" matches everything.
export function matchesTopics(event, topics) {
  return topics.some((spec) => {
    const [topic, key = '*'] = spec.split(':');
    if (topic !== '*' && topic !== event.Topic) return false;
    return key === '*' || key === event.Key;
  });
}
