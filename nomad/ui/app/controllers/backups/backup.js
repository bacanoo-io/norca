/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

export default class BackupsBackupController extends Controller {
  queryParams = ['tab', 'type', 'job', 'against'];

  @tracked tab = null;
  @tracked type = null;
  @tracked job = null;
  @tracked against = null;

  @action
  setTab(tab) {
    this.tab = tab === 'overview' ? null : tab;
  }

  @action
  setParams(params) {
    for (const [key, value] of Object.entries(params)) {
      this[key] = value || null;
    }
  }
}
