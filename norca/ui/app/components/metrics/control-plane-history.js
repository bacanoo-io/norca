/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import { fetchMetrics, fetchJSON } from 'nomad-ui/utils/metrics-history';

const JOB_STATES = [
  ['running', 'Running'],
  ['pending', 'Pending'],
  ['dead', 'Dead'],
];

/**
 * The control plane over time, compact, for the dashboard: the evaluation
 * broker, the plan queue and the blocked evaluations of the leader (operator
 * read), and the jobs by state of the namespaces the token may list (added
 * up). A part the token may not read is left out, the card when it may read
 * neither. Only the leader stores the scheduler series, so the history is
 * read from the leader (server_id) when it is known.
 *
 *   <Metrics::ControlPlaneHistory />
 */
export default class MetricsControlPlaneHistory extends Component {
  @service token;

  @tracked namespaces = null;
  @tracked leader = null;

  constructor() {
    super(...arguments);
    this.prepare.perform();
  }

  @task
  *prepare() {
    yield Promise.all([this.loadNamespaces(), this.loadLeader()]);
  }

  // The member name of the leader, which server_id takes.
  async loadLeader() {
    try {
      const [address, members] = await Promise.all([
        fetchJSON(this.token, '/v1/status/leader'),
        fetchJSON(this.token, '/v1/agent/members'),
      ]);
      const [bracketed, port] = String(address).split(/:(?=\d+$)/);
      // An IPv6 leader comes as [addr]:port; the members list it bare.
      const host = bracketed.replace(/^\[(.*)\]$/, '$1');
      const leader = (members.Members || []).find(
        (m) => (m.Addr || m.Address) === host && String(m.Tags?.port) === port
      );
      this.leader = leader ? leader.Name : null;
    } catch (e) {
      this.leader = null;
    }
  }

  // The namespaces with job series the token may read.
  async loadNamespaces() {
    try {
      const series = await fetchMetrics(this.token, 'series');
      const set = new Set(
        (series.Gauges || [])
          .filter((g) => g.Metric.startsWith('norca.jobs.') && g.Labels?.ns)
          .map((g) => g.Labels.ns)
      );
      this.namespaces = [...set].sort();
    } catch (e) {
      this.namespaces = [];
    }
  }

  get charts() {
    const sched = (name, metric, extra = {}) => ({
      name,
      metric: `norca.sched.${metric}`,
      step: true,
      ...extra,
    });
    const charts = [
      {
        id: 'scheduler',
        title: 'Scheduler (leader)',
        axes: [{ unit: 'count' }],
        series: [
          sched('Ready', 'broker_ready'),
          sched('Unacked', 'broker_unacked'),
          sched('Pending', 'broker_pending', { dashed: true }),
          sched('Plan queue', 'plan_queue_depth'),
          sched('Blocked', 'blocked_total', { dashed: true }),
        ],
      },
    ];
    const namespaces = this.namespaces || [];
    if (namespaces.length) {
      charts.push({
        id: 'jobs',
        title:
          namespaces.length === 1
            ? `Jobs in ${namespaces[0]}`
            : `Jobs in ${namespaces.length} namespaces`,
        axes: [{ unit: 'count' }],
        series: JOB_STATES.map(([state, label]) => ({
          name: label,
          metric: `norca.jobs.${state}`,
          labels: namespaces.map((ns) => ({ ns })),
          step: true,
        })),
      });
    }
    return charts;
  }
}
