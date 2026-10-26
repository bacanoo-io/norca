/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// One connection to /v1/event/stream for one region.
//
// The stream is newline-delimited JSON: `{"Index": N, "Events": [...]}` per
// batch and `{}` as heartbeat. The connection remembers the index of the last
// batch it delivered and, after a disconnect, resumes from the next index with
// `exact=true`, so no event is skipped silently. When the server can no longer
// guarantee that (410 Gone: the index dropped out of its buffer) the
// connection reports a resync: subscribers refetch their state and the
// connection continues from the index of that state, which they report back
// through `resumeFrom`.

export const STATES = {
  idle: 'idle',
  connecting: 'connecting',
  open: 'open',
  retrying: 'retrying',
  closed: 'closed',
  forbidden: 'forbidden',
};

const MIN_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30000;

export function buildStreamURL({ topics, index, exact, region, namespace }) {
  const params = new URLSearchParams();
  for (const topic of topics || []) params.append('topic', topic);
  if (index > 0) {
    params.set('index', String(index));
    if (exact) params.set('exact', 'true');
  }
  if (region) params.set('region', region);
  if (namespace) params.set('namespace', namespace);
  const qs = params.toString();
  return `/v1/event/stream${qs ? `?${qs}` : ''}`;
}

// Splits a chunked text stream into complete lines; returns the parsed
// objects and keeps an unterminated tail for the next chunk.
export class NDJSONParser {
  buffer = '';

  push(text) {
    this.buffer += text;
    const lines = this.buffer.split('\n');
    this.buffer = lines.pop();
    const out = [];
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      try {
        out.push(JSON.parse(trimmed));
      } catch (e) {
        // A malformed line is skipped; the next batch carries a new index.
      }
    }
    return out;
  }
}

export default class EventStreamConnection {
  state = STATES.idle;
  lastIndex = 0;
  attempts = 0;
  controller = null;
  timer = null;
  stopped = false;

  /**
   * @param {object} options
   * @param {string[]} options.topics
   * @param {string} [options.region]
   * @param {string} [options.namespace]
   * @param {boolean} [options.replay] start with the events the server still buffers
   * @param {(url: string, init: object) => Promise<Response>} options.request
   * @param {(batch: {Index: number, Events: object[]}) => void} options.onBatch
   * @param {() => (number|Promise<number>)} [options.onResync] refetch hook; resolves to the index to continue after
   * @param {(state: string) => void} [options.onStateChange]
   * @param {(fn: Function, ms: number) => any} [options.schedule]
   */
  constructor(options) {
    this.options = options;
    this.schedule = options.schedule || ((fn, ms) => setTimeout(fn, ms));
    this.cancelSchedule = options.cancelSchedule || ((t) => clearTimeout(t));
  }

  setState(state) {
    if (this.state === state) return;
    this.state = state;
    this.options.onStateChange?.(state);
  }

  start() {
    this.stopped = false;
    this.connect();
  }

  stop() {
    this.stopped = true;
    if (this.timer) {
      this.cancelSchedule(this.timer);
      this.timer = null;
    }
    this.controller?.abort();
    this.controller = null;
    this.setState(STATES.closed);
  }

  // Continue after `index` (inclusive of later events). Used after a resync
  // with the X-Nomad-Index of the refetched state.
  resumeFrom(index) {
    if (index > this.lastIndex) this.lastIndex = index;
  }

  get url() {
    const { topics, region, namespace, replay } = this.options;
    if (this.lastIndex > 0) {
      const index = this.lastIndex + 1;
      return buildStreamURL({ topics, index, exact: true, region, namespace });
    }
    // index=1 without exact: start with what the server still buffers.
    const index = replay ? 1 : 0;
    return buildStreamURL({ topics, index, exact: false, region, namespace });
  }

  async connect() {
    if (this.stopped) return;
    this.timer = null;
    this.setState(STATES.connecting);
    const controller = new AbortController();
    this.controller = controller;

    let response;
    try {
      response = await this.options.request(this.url, {
        signal: controller.signal,
      });
    } catch (e) {
      if (controller.signal.aborted || this.stopped) return;
      return this.retry();
    }

    if (this.stopped || controller.signal.aborted) return;

    if (response.status === 410) {
      // The resume index is gone: subscribers refetch their state and the
      // stream continues right after the index of that state (or from the
      // head when no subscriber reports one).
      this.lastIndex = 0;
      this.setState(STATES.retrying);
      let index = 0;
      try {
        index = Number(await this.options.onResync?.()) || 0;
      } catch (e) {
        index = 0;
      }
      if (this.stopped) return;
      this.resumeFrom(index);
      return this.reconnectSoon(0);
    }

    if (response.status === 401 || response.status === 403) {
      // The token may not read these topics; wait for a new subscription.
      this.setState(STATES.forbidden);
      return;
    }

    if (!response.ok) {
      return this.retry();
    }

    this.setState(STATES.open);
    this.attempts = 0;

    try {
      await this.consume(response, controller);
    } catch (e) {
      // network error mid-stream: fall through to reconnect
    }
    if (this.stopped || controller.signal.aborted) return;
    // The server ended the stream (e.g. leader change): resume.
    this.reconnectSoon(MIN_BACKOFF_MS);
  }

  async consume(response, controller) {
    const parser = new NDJSONParser();
    const reader = response.body?.getReader?.();
    if (!reader) {
      // Environments without streaming bodies (the test mock server)
      // deliver the whole response at once.
      const text = await response.text();
      this.deliver(parser.push(text + '\n'));
      return;
    }
    const decoder = new TextDecoder();
    for (;;) {
      const { value, done } = await reader.read();
      if (done || controller.signal.aborted) break;
      this.deliver(parser.push(decoder.decode(value, { stream: true })));
    }
  }

  deliver(messages) {
    for (const message of messages) {
      if (!message || !message.Index || !Array.isArray(message.Events))
        continue;
      if (message.Index <= this.lastIndex) continue;
      this.lastIndex = message.Index;
      this.options.onBatch(message);
    }
  }

  retry() {
    this.attempts += 1;
    const backoff = Math.min(
      MAX_BACKOFF_MS,
      MIN_BACKOFF_MS * 2 ** Math.min(this.attempts - 1, 5)
    );
    this.reconnectSoon(backoff);
  }

  reconnectSoon(ms) {
    if (this.stopped) return;
    this.setState(STATES.retrying);
    this.timer = this.schedule(() => this.connect(), ms);
  }
}
