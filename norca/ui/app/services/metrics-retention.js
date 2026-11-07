/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { rangesFor, isHostMetric } from 'nomad-ui/utils/metrics-history';

/**
 * The retentions of the metrics store of the server the UI reads, as the
 * history and series responses report them (no operator read needed): of the
 * main store, and of the hosts store, which keeps the host metrics
 * (norca.node.*, norca.server.*, norca.bus.*) for host_retention. The range
 * selectors of the history charts offer only the ranges the store of their
 * metrics holds; until a response tells the retention, all ranges.
 */
export default class MetricsRetentionService extends Service {
  // Nanoseconds, as the API encodes a duration; null while not known.
  @tracked retention = null;
  @tracked hostRetention = null;

  // note takes the retentions of a history or series response. A history
  // answers with the retention of the store that keeps its metric.
  note(response) {
    if (!response || !response.Enabled) return;
    if (response.Retention > 0) {
      if (isHostMetric(response.Metric)) {
        this.setHostRetention(response.Retention);
      } else if (response.Retention !== this.retention) {
        this.retention = response.Retention;
      }
    }
    if (response.HostRetention > 0) {
      this.setHostRetention(response.HostRetention);
    }
  }

  setHostRetention(retention) {
    if (retention !== this.hostRetention) {
      this.hostRetention = retention;
    }
  }

  get ranges() {
    return rangesFor(this.retention);
  }

  get hostRanges() {
    return rangesFor(this.hostRetention);
  }
}
