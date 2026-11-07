/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

/**
 * The host history of a client node from the metrics store of the servers
 * (the statistics the client reports with its usage): CPU, memory, and the
 * file system of the allocation directory. Needs node read; without it the
 * card is left out.
 *
 *   <Metrics::NodeHistory @node={{this.model}} />
 */
export default class MetricsNodeHistory extends Component {
  // The labels of the series of the node: the server stores them with the
  // datacenter, node pool and node class of the node in its state.
  get labels() {
    const node = this.args.node;
    return {
      node: node.id,
      dc: node.datacenter,
      pool: node.nodePool,
      class: node.nodeClass,
    };
  }

  get charts() {
    const labels = this.labels;
    return [
      {
        id: 'cpu',
        title: 'CPU',
        axes: [{ unit: 'percent', max: 100 }],
        series: [
          { name: 'Used', metric: 'norca.node.cpu_used', labels },
          {
            name: 'Peak',
            metric: 'norca.node.cpu_used',
            labels,
            prop: 'Max',
            area: true,
          },
        ],
      },
      {
        id: 'memory',
        title: 'Memory',
        axes: [{ unit: 'mib' }],
        series: [
          { name: 'Used', metric: 'norca.node.memory_used_mb', labels },
          {
            name: 'Total',
            metric: 'norca.node.memory_total_mb',
            labels,
            dashed: true,
          },
        ],
      },
      {
        id: 'disk',
        title: 'Allocation directory',
        axes: [{ unit: 'mib' }, { unit: 'percent', max: 100 }],
        series: [
          { name: 'Used', metric: 'norca.node.disk_used_mb', labels },
          {
            name: 'Size',
            metric: 'norca.node.disk_total_mb',
            labels,
            dashed: true,
          },
          {
            name: 'Inodes used',
            metric: 'norca.node.inodes_used_percent',
            labels,
            axis: 1,
            optional: true,
          },
        ],
      },
    ];
  }
}
