/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import legacyTransitionTo from 'nomad-ui/utils/legacy-transition';

export default class IndexRoute extends Route {
  redirect() {
    legacyTransitionTo(this, 'csi.volumes');
  }
}
