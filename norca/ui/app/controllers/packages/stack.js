/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

// operation: the operation the page shows; the newest when none.
export default class PackagesStackController extends Controller {
  queryParams = ['operation'];

  @tracked operation = null;

  @action
  setOperation(name) {
    this.operation = name || null;
  }
}
