/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

export default class BackupsJobHistoryController extends Controller {
  queryParams = ['job', 'namespace'];

  @tracked job = null;
  @tracked namespace = null;

  @action
  search({ job, namespace }) {
    this.job = job;
    this.namespace = namespace;
  }
}
