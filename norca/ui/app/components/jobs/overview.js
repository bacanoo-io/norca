/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { jobOwner } from 'nomad-ui/utils/job-owner';

export default class JobsOverview extends Component {
  @service system;

  // Errors of the operations of the status panel (promote, fail).
  @tracked error = null;

  @action
  handleError(error) {
    this.error = error;
    this.args.onError?.(error);
  }

  @action
  clearError() {
    this.error = null;
  }

  get owner() {
    return jobOwner(this.args.job);
  }

  get multiregionRegions() {
    return this.args.job.multiregionRegions || [];
  }

  get payload() {
    return this.args.job.decodedPayload;
  }

  get payloadJSON() {
    try {
      return JSON.stringify(JSON.parse(this.payload), null, 2);
    } catch (e) {
      return null;
    }
  }
}
