/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

export default class NetworkIndexController extends Controller {
  queryParams = ['tab'];

  @tracked tab = null;

  @action
  setTab(tab) {
    this.tab = tab === 'intentions' ? null : tab;
  }
}
