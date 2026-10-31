/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';

export default class CsiPluginsPluginController extends Controller {
  get plugin() {
    return this.model;
  }
}
