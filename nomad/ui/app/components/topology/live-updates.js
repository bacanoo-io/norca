/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import config from 'nomad-ui/config/environment';

// Events come in bursts (a deployment moves many allocations); one reload
// per burst is enough for the topology (tests do not wait that long).
export const RELOAD_DEBOUNCE_MS = config.environment === 'test' ? 10 : 5000;

/**
 * Reloads the topology when the event stream reports a change of a client
 * or an allocation in the active region, instead of polling the large
 * allocation list. Renders the live state.
 *
 *   <Topology::LiveUpdates @onChange={{this.refresh}} />
 */
export default class TopologyLiveUpdates extends Component {
  @service eventStream;
  @service system;

  timer = null;

  constructor() {
    super(...arguments);
    this.region = this.system.activeRegion || '';
    this.unsubscribe = this.eventStream.subscribe({
      topics: ['Node', 'Allocation'],
      region: this.region,
      namespace: '*',
      onEvents: () => this.schedule(),
      onResync: () => this.schedule(),
    });
  }

  get state() {
    return this.eventStream.stateFor(this.region, '*');
  }

  get isLive() {
    return this.state === 'open';
  }

  schedule() {
    if (this.timer) return;
    this.timer = setTimeout(() => {
      this.timer = null;
      if (!this.isDestroying) this.args.onChange?.();
    }, this.args.debounce ?? RELOAD_DEBOUNCE_MS);
  }

  willDestroy() {
    super.willDestroy(...arguments);
    clearTimeout(this.timer);
    this.unsubscribe?.();
  }
}
