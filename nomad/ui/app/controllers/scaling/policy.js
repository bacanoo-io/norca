/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

export default class ScalingPolicyController extends Controller {
  queryParams = ['namespace'];

  @tracked namespace = null;
}
