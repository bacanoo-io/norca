/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Helper from '@ember/component/helper';
import config from 'nomad-ui/config/environment';

// Base class of the relative time helpers (moment-from-now, moment-to-now,
// moment-from). They replace the ember-moment addon. With `interval=<ms>`
// the helper recomputes periodically; timers are disabled in tests so that
// test settledness is not affected.
export default class RelativeTimeHelper extends Helper {
  timer = null;

  compute(positional, named = {}) {
    this.clearTimer();
    if (named.interval && config.environment !== 'test') {
      this.timer = setTimeout(() => {
        this.timer = null;
        if (!this.isDestroying && !this.isDestroyed) this.recompute();
      }, named.interval);
    }
    return this.format(positional, named);
  }

  // eslint-disable-next-line no-unused-vars
  format(positional, named) {
    return '';
  }

  clearTimer() {
    if (this.timer) {
      clearTimeout(this.timer);
      this.timer = null;
    }
  }

  willDestroy() {
    this.clearTimer();
    super.willDestroy(...arguments);
  }
}
