/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class JobsServiceHealth extends Component {
  get counts() {
    const instances = this.args.instances || [];
    const list =
      typeof instances.toArray === 'function' ? instances.toArray() : instances;
    const counts = {
      total: list.length,
      ready: 0,
      passing: 0,
      warning: 0,
      critical: 0,
      unknown: 0,
    };
    list.forEach((instance) => {
      if (instance.ready) counts.ready++;
      const health = instance.health;
      if (
        health === 'passing' ||
        health === 'warning' ||
        health === 'critical'
      ) {
        counts[health]++;
      } else {
        counts.unknown++;
      }
    });
    return counts;
  }
}
