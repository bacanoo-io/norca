/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import legacyTransitionTo from 'nomad-ui/utils/legacy-transition';

export default class IndexRoute extends Route {
  redirect() {
    legacyTransitionTo(this, 'csi.volumes');
  }
}
