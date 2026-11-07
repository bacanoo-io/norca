/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import moment from 'moment';
import { fetchJSON, MetricsForbidden } from 'nomad-ui/utils/metrics-history';
import {
  formatBytesValue,
  formatDuration,
} from 'nomad-ui/utils/metrics-format';

// mostCommon returns the value most of the rows have (the first on a tie).
function mostCommon(values) {
  const counts = new Map();
  values.forEach((v) => counts.set(v, (counts.get(v) || 0) + 1));
  let best;
  let bestCount = 0;
  counts.forEach((count, v) => {
    if (count > bestCount) {
      best = v;
      bestCount = count;
    }
  });
  return best;
}

/**
 * The metrics store of every server (each keeps its own history): the
 * retention in effect, the oldest point, the disk use against max_disk_size,
 * the series against max_series, and what it dropped or forgot; and its hosts
 * store (the host metrics, norca.node.*, norca.server.*, norca.bus.*): the
 * retention and the disk use against host_max_disk_size. A server whose
 * retentions or disk caps differ from the other servers of its region is
 * marked: the same query answers with a different range depending on the
 * server it reaches. Needs operator read; without it the card is left out.
 *
 *   <Metrics::StoreStatus @servers={{this.agents}} />
 */
export default class MetricsStoreStatus extends Component {
  @service token;

  @tracked statuses = [];
  @tracked state = 'loading'; // loading, ready, forbidden

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  get servers() {
    const servers = this.args.servers;
    if (!servers) return [];
    return typeof servers.toArray === 'function'
      ? servers.toArray()
      : [...servers];
  }

  get isShown() {
    return this.state !== 'forbidden';
  }

  @task({ restartable: true })
  *load() {
    const results = yield Promise.all(
      this.servers.map(async (server) => {
        try {
          const body = await fetchJSON(
            this.token,
            '/v1/operator/metrics-store',
            { server_id: server.name, region: server.region }
          );
          return { server, status: body.Status || {} };
        } catch (e) {
          return {
            server,
            forbidden: e instanceof MetricsForbidden,
            error: e.message,
          };
        }
      })
    );
    if (!results.length || results.every((r) => r.forbidden)) {
      this.state = 'forbidden';
      return;
    }
    this.statuses = results;
    this.state = 'ready';
  }

  // The values most servers of each region have.
  get common() {
    const byRegion = new Map();
    this.statuses
      .filter((r) => r.status && r.status.Enabled)
      .forEach((r) => {
        const region = r.server.region || '';
        if (!byRegion.has(region)) byRegion.set(region, []);
        byRegion.get(region).push(r.status);
      });
    const common = new Map();
    byRegion.forEach((list, region) =>
      common.set(region, {
        retention: mostCommon(list.map((s) => s.Retention)),
        maxDiskSize: mostCommon(list.map((s) => s.MaxDiskSize)),
        hostRetention: mostCommon(list.map((s) => s.Hosts?.Retention)),
        hostMaxDiskSize: mostCommon(list.map((s) => s.Hosts?.MaxDiskSize)),
        count: list.length,
      })
    );
    return common;
  }

  get rows() {
    const common = this.common;
    return this.statuses.map(({ server, status, error }) => {
      const row = { name: server.name, region: server.region, error };
      if (!status) return row;
      row.enabled = status.Enabled;
      if (!status.Enabled) return row;
      const c = common.get(server.region || '') || {};
      const several = c.count > 1;
      const oldest = status.Oldest && moment(status.Oldest);
      const hosts = status.Hosts;
      return {
        ...row,
        retention: formatDuration(status.Retention),
        retentionDiffers: several && status.Retention !== c.retention,
        commonRetention: formatDuration(c.retention),
        oldest:
          oldest && oldest.isValid() && oldest.year() > 1
            ? oldest.fromNow()
            : '–',
        oldestTitle: oldest && oldest.isValid() ? oldest.format() : '',
        disk: formatBytesValue(status.DiskBytes),
        maxDisk: formatBytesValue(status.MaxDiskSize),
        diskDiffers: several && status.MaxDiskSize !== c.maxDiskSize,
        commonMaxDisk: formatBytesValue(c.maxDiskSize),
        diskValue: status.DiskBytes,
        diskMax: status.MaxDiskSize || 1,
        diskColor:
          status.MaxDiskSize && status.DiskBytes / status.MaxDiskSize > 0.9
            ? 'orange'
            : 'blue',
        series: status.Series,
        maxSeries: status.MaxSeries,
        seriesFull: status.MaxSeries > 0 && status.Series >= status.MaxSeries,
        droppedReports: status.DroppedReports || 0,
        droppedGroups: status.DroppedGroups || 0,
        forgotten: status.ForgottenSeries || 0,
        hosts: !!hosts,
        hostRetention: hosts ? formatDuration(hosts.Retention) : '',
        hostRetentionDiffers:
          !!hosts && several && hosts.Retention !== c.hostRetention,
        commonHostRetention: formatDuration(c.hostRetention),
        hostDisk: hosts ? formatBytesValue(hosts.DiskBytes) : '',
        hostMaxDisk: hosts ? formatBytesValue(hosts.MaxDiskSize) : '',
        hostDiskDiffers:
          !!hosts && several && hosts.MaxDiskSize !== c.hostMaxDiskSize,
        commonHostMaxDisk: formatBytesValue(c.hostMaxDiskSize),
      };
    });
  }

  get differing() {
    return this.rows.filter(
      (r) =>
        r.retentionDiffers ||
        r.diskDiffers ||
        r.hostRetentionDiffers ||
        r.hostDiskDiffers
    );
  }

  get dropping() {
    return this.rows.filter((r) => r.droppedGroups > 0 || r.seriesFull);
  }
}
