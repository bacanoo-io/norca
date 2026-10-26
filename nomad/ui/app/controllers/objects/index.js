/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

export default class ObjectsIndexController extends Controller {
  queryParams = ['kind'];

  @tracked kind = null;

  @action
  setKind(kind) {
    this.kind = kind;
  }
}
