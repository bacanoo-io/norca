/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action, get } from '@ember/object';
import { inject as service } from '@ember/service';

export default class AllocationsTaskRow extends Component {
  @service router;

  // The name cell spans every visible column except CPU and memory.
  get nameSpan() {
    const shown = this.args.r.shown || {};
    const metrics = (shown.cpu ? 1 : 0) + (shown.memory ? 1 : 0);
    return Math.max(1, this.args.r.colspan - metrics);
  }

  get taskStats() {
    const stats = this.args.stats;
    if (!stats || !this.args.taskState.isRunning) return null;
    const tasks = get(stats, 'tasks') || [];
    return tasks.find((t) => t.task === this.args.taskState.name) || null;
  }

  @action
  gotoTask() {
    this.router.transitionTo(
      'allocations.allocation.task',
      this.args.taskState.allocation,
      this.args.taskState
    );
  }

  @action
  openLogs() {
    this.args.onSetActiveTask?.(this.args.active ? null : this.args.taskState);
  }
}
