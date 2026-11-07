/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

/**
 * The Raft log and the system bus of one server over time, from the metrics
 * store of that server (each server samples its own; the history is read
 * from it with server_id). Needs operator read; without it the card is left
 * out.
 *
 *   <Metrics::ServerHistory @server={{this.model}} />
 */
export default class MetricsServerHistory extends Component {
  get charts() {
    return [
      {
        id: 'raft-db',
        title: 'Raft log on disk',
        axes: [{ unit: 'bytes' }],
        series: [{ name: 'Size', metric: 'norca.server.raft_db_bytes' }],
      },
      {
        id: 'raft-contact',
        title: 'Contact with the leader and apply lag',
        axes: [{ unit: 'seconds' }, { unit: 'count' }],
        series: [
          {
            name: 'Last contact',
            metric: 'norca.server.raft_last_contact_seconds',
          },
          {
            name: 'Last contact, peak',
            metric: 'norca.server.raft_last_contact_seconds',
            prop: 'Max',
            area: true,
          },
          {
            name: 'Commit lag',
            metric: 'norca.server.raft_commit_lag',
            axis: 1,
            step: true,
          },
          {
            name: 'FSM pending',
            metric: 'norca.server.raft_fsm_pending',
            axis: 1,
            step: true,
            dashed: true,
          },
        ],
      },
      {
        id: 'bus-streams',
        title: 'System bus streams',
        axes: [{ unit: 'bytes' }, { unit: 'count' }],
        series: [
          { name: 'JetStream bytes', metric: 'norca.bus.jetstream_bytes' },
          {
            name: 'Messages',
            metric: 'norca.bus.jetstream_messages',
            axis: 1,
            dashed: true,
          },
        ],
      },
      {
        id: 'term-routes',
        title: 'Raft term and bus routes',
        axes: [{ unit: 'count' }, { unit: 'count' }],
        series: [
          { name: 'Raft term', metric: 'norca.server.raft_term', step: true },
          {
            name: 'Routes to servers',
            metric: 'norca.bus.routes',
            axis: 1,
            step: true,
          },
        ],
      },
    ];
  }
}
