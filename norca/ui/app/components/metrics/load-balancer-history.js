/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

/**
 * The health of a load balancer (norca.io/LoadBalancer) over time, from the
 * metrics store of the servers: its healthy targets and whether it was
 * ready. Needs read on the LoadBalancer objects of the namespace; without it
 * the card is left out.
 *
 *   <Metrics::LoadBalancerHistory @namespace="infra" @name="ingress" />
 */
export default class MetricsLoadBalancerHistory extends Component {
  get charts() {
    const labels = { ns: this.args.namespace, name: this.args.name };
    return [
      {
        id: 'targets',
        title: 'Healthy targets',
        axes: [{ unit: 'count' }],
        series: [
          {
            name: 'Healthy targets',
            metric: 'norca.lb.targets',
            labels,
            step: true,
          },
        ],
      },
      {
        id: 'ready',
        title: 'Ready (share of the time)',
        axes: [{ unit: 'ratio', max: 1 }],
        series: [
          {
            name: 'Ready',
            metric: 'norca.lb.ready',
            labels,
            step: true,
            area: true,
          },
        ],
      },
    ];
  }
}
