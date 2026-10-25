/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { debounce, cancel } from '@ember/runloop';

/**
 * The count of a task group with the decrement and increment buttons of
 * manual scaling (task groups with a scaling block). The new count is sent
 * after @debounce milliseconds without a click (default 500).
 *
 *   <Jobs::ScaleControls @taskGroup={{taskGroup}} />
 */
export default class JobsScaleControls extends Component {
  @service can;

  @tracked pending = null;
  timer = null;

  get count() {
    return this.pending ?? this.args.taskGroup.count;
  }

  get scaling() {
    return this.args.taskGroup.scaling;
  }

  get namespace() {
    return this.args.taskGroup.get?.('job.namespace.name');
  }

  get runningDeployment() {
    return this.args.taskGroup.get?.('job.runningDeployment');
  }

  get cannotScale() {
    return this.can.cannot('scale job', null, { namespace: this.namespace });
  }

  get disabledReason() {
    if (this.cannotScale) return "You aren't allowed to scale task groups";
    if (this.runningDeployment)
      return 'You cannot scale task groups during a deployment';
    return null;
  }

  get isMinimum() {
    const min = this.scaling?.min;
    return min != null && this.count <= min;
  }

  get isMaximum() {
    const max = this.scaling?.max;
    return max != null && this.count >= max;
  }

  @action
  countDown() {
    if (this.isMinimum) return;
    this.setCount(this.count - 1);
  }

  @action
  countUp() {
    if (this.isMaximum) return;
    this.setCount(this.count + 1);
  }

  setCount(count) {
    this.pending = count;
    this.timer = debounce(this, this.send, count, this.args.debounce ?? 500);
  }

  send(count) {
    if (this.isDestroying || this.isDestroyed) return;
    Promise.resolve(this.args.taskGroup.scale(count))
      .catch((error) => this.args.onError?.(error))
      .finally(() => {
        if (!this.isDestroying && !this.isDestroyed) this.pending = null;
      });
  }

  willDestroy() {
    super.willDestroy(...arguments);
    cancel(this.timer);
  }
}
