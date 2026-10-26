/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class JobsClientStatusRow extends Component {
  get row() {
    return this.args.row;
  }

  get shouldDisplayAllocationSummary() {
    return this.args.row.jobStatus !== 'notScheduled';
  }

  get allocationSummaryPlaceholder() {
    switch (this.args.row.jobStatus) {
      case 'notScheduled':
        return 'Not Scheduled';
      default:
        return '';
    }
  }

  get humanizedJobStatus() {
    switch (this.args.row.jobStatus) {
      case 'notScheduled':
        return 'not scheduled';
      default:
        return this.args.row.jobStatus;
    }
  }

  get jobStatusClass() {
    switch (this.args.row.jobStatus) {
      case 'notScheduled':
        return 'not-scheduled';
      default:
        return this.args.row.jobStatus;
    }
  }

  // The allocation states of the job on the client, for the distribution.
  get allocationData() {
    const counts = {
      queued: 0,
      starting: 0,
      running: 0,
      complete: 0,
      unknown: 0,
      failed: 0,
      lost: 0,
    };
    const allocations = this.args.row.allocations || [];
    switch (this.args.row.jobStatus) {
      case 'queued':
        counts.queued = allocations.length;
        break;
      case 'starting':
        counts.starting = allocations.length;
        break;
      default:
        for (const alloc of allocations) {
          if (alloc.clientStatus in counts) counts[alloc.clientStatus]++;
        }
    }
    return Object.entries(counts).map(([key, value]) => ({
      key,
      label: key.charAt(0).toUpperCase() + key.slice(1),
      value,
    }));
  }
}
