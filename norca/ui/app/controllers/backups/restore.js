/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

export default class BackupsRestoreController extends Controller {
  queryParams = ['mode'];

  @tracked mode = null;
}
