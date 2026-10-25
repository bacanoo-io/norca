/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { summarizeEvent } from 'nomad-ui/utils/event-summary';

const LIMIT = 8;

// Notifications from the event stream: failed deployments and allocations,
// blocked evaluations, clients going down or draining. The badge marks
// events that arrived since the menu was last opened.
export default class FrameNotifications extends Component {
  @service eventStream;
  @service frame;
  @service abilities;

  @tracked seenIndex = 0;

  get topics() {
    const topics = ['Deployment', 'Evaluation', 'Allocation'];
    if (this.abilities.can('read client')) topics.push('Node');
    return topics;
  }

  // Re-subscribes whenever the region or namespace scope changes.
  subscribe = modifier((element, [regions, namespace, topics]) => {
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({ topics, region, namespace })
    );
    return () => unsubscribers.forEach((unsubscribe) => unsubscribe());
  });

  get items() {
    return this.eventStream.recentEvents
      .map(summarizeEvent)
      .filter((item) => item.notable)
      .slice(0, LIMIT);
  }

  get hasUnseen() {
    return this.items.some((item) => item.index > this.seenIndex);
  }

  @action
  markSeen() {
    this.seenIndex = Math.max(0, ...this.items.map((item) => item.index || 0));
  }
}
