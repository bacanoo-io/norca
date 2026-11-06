/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import moment from 'moment';
import {
  META,
  nodeBadges,
  nodeGroupOf,
  replacementTimes,
} from 'nomad-ui/utils/node-scaling';

function when(date) {
  if (!date) return null;
  return `${moment(date).format('YYYY-MM-DD HH:mm')} (${moment(
    date
  ).fromNow()})`;
}

/**
 * The node group of a client the node scaling operator created: the group
 * as a link to its object, the instance, the state badges, the revision and
 * generation it was created with, its age, the expiry of its certificate and
 * when the operator replaces it by its age (the soft limit with the jitter of
 * the node, and the hard limit). Nothing for a node added by hand.
 */
export default class ClientsNodeGroupCard extends Component {
  @service nodeScaling;

  constructor() {
    super(...arguments);
    this.nodeScaling.refresh();
  }

  get meta() {
    return this.args.node?.meta?.raw || {};
  }

  get groupName() {
    const n = this.args.node;
    if (!n) return null;
    return nodeGroupOf(
      this.nodeScaling.loaded ? this.nodeScaling.index : null,
      { id: n.id, name: n.name },
      this.meta
    );
  }

  get badges() {
    const n = this.args.node;
    return nodeBadges(
      this.nodeScaling.loaded ? this.nodeScaling.index : null,
      { id: n.id, name: n.name },
      this.meta
    );
  }

  get items() {
    const m = this.meta;
    const group = this.nodeScaling.group(this.groupName);
    const times = replacementTimes(
      m,
      group?.Spec,
      m[META.instance] || this.args.node?.name
    );
    const items = [];
    if (m[META.instance])
      items.push({ title: 'Instance', value: m[META.instance] });
    if (times.created)
      items.push({ title: 'Created', value: when(times.created) });
    if (m[META.revision])
      items.push({ title: 'Template revision', value: m[META.revision] });
    if (m[META.generation] !== undefined)
      items.push({ title: 'Rolling generation', value: m[META.generation] });
    if (times.expires)
      items.push({ title: 'Certificate expires', value: when(times.expires) });
    if (m[META.protect] === 'true') {
      items.push({
        title: 'Replacement',
        value: 'protected (norca.scale.protect)',
      });
    } else {
      if (times.soft)
        items.push({ title: 'Replaced by age from', value: when(times.soft) });
      if (times.hard)
        items.push({
          title: 'Replaced at the latest',
          value: when(times.hard),
        });
    }
    if (m[META.replace] === 'true')
      items.push({
        title: 'Marked for replacement',
        value: 'yes (norca.scale.replace)',
      });
    if (m[META.leaving] === 'true')
      items.push({
        title: 'Load balancers',
        value: 'leaving them before its drain (norca.lb.leaving)',
      });
    return items;
  }
}
