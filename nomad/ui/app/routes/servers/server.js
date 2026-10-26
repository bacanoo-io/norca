/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import notifyError from 'nomad-ui/utils/notify-error';

export default class ServerRoute extends Route {
  @service store;

  // Ember 6 no longer looks records up from the dynamic segment by itself.
  model(params) {
    return this.store
      .findRecord('agent', params.agent_id)
      .catch(notifyError(this));
  }
}
