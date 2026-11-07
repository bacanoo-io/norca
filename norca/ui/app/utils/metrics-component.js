/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import {
  rangesFor,
  clampRange,
  fetchMetrics,
} from 'nomad-ui/utils/metrics-history';

/**
 * The base of the history charts of the metrics store: the range selector,
 * limited to the retention of the store that answers, and the requests.
 *
 * With @serverId the requests go to that server (`server_id`) and its own
 * retention limits the ranges; without it, the retention of the server the UI
 * reads (shared by all charts through the metrics-retention service).
 * A subclass sets `range` (the default range id) and a `load` task, which
 * reads the history of `this.selectedRange` with `this.fetch`.
 */
export default class MetricsComponent extends Component {
  @service token;
  @service metricsRetention;

  @tracked range = '24h';
  @tracked serverRetention = null;

  get serverId() {
    return this.args.serverId;
  }

  get retention() {
    return this.serverId
      ? this.serverRetention
      : this.metricsRetention.retention;
  }

  get ranges() {
    return rangesFor(this.retention);
  }

  // The selected range, or the longest the store holds when the selected
  // one is longer than the retention.
  get selectedRange() {
    return clampRange(this.range, this.ranges);
  }

  get rangeId() {
    return this.selectedRange.id;
  }

  get tabs() {
    return this.ranges.map((r) => ({ id: r.id, label: r.label }));
  }

  @action
  selectRange(id) {
    this.range = id;
    this.load.perform();
  }

  // fetch reads /v1/metrics/<path> (of @serverId, in @region when given) and
  // notes the retention of the answer.
  async fetch(path, params = {}) {
    const response = await fetchMetrics(this.token, path, {
      ...params,
      server_id: this.serverId,
      region: this.args.region,
    });
    if (this.serverId) {
      if (response && response.Enabled && response.Retention > 0) {
        this.serverRetention = response.Retention;
      }
    } else {
      this.metricsRetention.note(response);
    }
    return response;
  }
}
