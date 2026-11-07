/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { rangesFor } from 'nomad-ui/utils/metrics-history';

/**
 * The retention of the metrics store of the server the UI reads, as the
 * history and series responses report it (no operator read needed). The
 * range selectors of the history charts offer only the ranges it holds; until
 * a response tells the retention, all ranges.
 */
export default class MetricsRetentionService extends Service {
  // Nanoseconds, as the API encodes a duration; null while not known.
  @tracked retention = null;

  // note takes the retention of a history or series response.
  note(response) {
    if (response && response.Enabled && response.Retention > 0) {
      if (response.Retention !== this.retention) {
        this.retention = response.Retention;
      }
    }
  }

  get ranges() {
    return rangesFor(this.retention);
  }
}
