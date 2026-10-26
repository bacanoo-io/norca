/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { jobOwner } from 'nomad-ui/utils/job-owner';

export default class JobsListRow extends Component {
  get owner() {
    return jobOwner(this.args.job);
  }

  get childrenData() {
    const job = this.args.job;
    return [
      { key: 'pending', label: 'Pending', value: job.pendingChildren || 0 },
      { key: 'running', label: 'Running', value: job.runningChildren || 0 },
      { key: 'dead', label: 'Dead', value: job.deadChildren || 0 },
    ];
  }
}
