/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

const TABS = [
  { id: 'intentions', label: 'Intentions' },
  { id: 'policies', label: 'Network policies' },
  { id: 'admin', label: 'Admin network policies' },
];

/**
 * Intentions and network rules: service intentions (mTLS identities),
 * namespace network policies and cluster-wide admin network policies of the
 * active region.
 */
export default class NetworkPage extends Component {
  @service frame;
  @service system;

  tabs = TABS;

  get tab() {
    return TABS.some((t) => t.id === this.args.tab)
      ? this.args.tab
      : 'intentions';
  }

  get region() {
    return this.system.activeRegion || '';
  }

  get scopeLabel() {
    const ns = this.frame.namespace || '*';
    return `${this.region || 'Local region'}, ${
      ns === '*' ? 'all namespaces' : `namespace ${ns}`
    }`;
  }
}
