/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { nodeBadges } from 'nomad-ui/utils/node-scaling';

/**
 * The badges of a client that the node scaling operator created: Autoscaled,
 * and its state from the status of its node group (Removing, Replacing,
 * Outdated, Unverified...). @node is the node model.
 */
export default class ClientsNodeScalingBadges extends Component {
  @service nodeScaling;

  constructor() {
    super(...arguments);
    this.nodeScaling.refresh();
  }

  get badges() {
    const node = this.args.node;
    if (!node) return [];
    return nodeBadges(
      this.nodeScaling.loaded ? this.nodeScaling.index : null,
      { id: node.id, name: node.name },
      node.meta?.raw
    );
  }
}
