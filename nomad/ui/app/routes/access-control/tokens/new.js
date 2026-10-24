/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';

export default class AccessControlTokensNewRoute extends Route {
  @service can;
  @service router;
  @service store;

  beforeModel() {
    if (this.can.cannot('write token')) {
      this.router.transitionTo('/access-control/tokens');
    }
  }

  model() {
    return {
      token: this.store.createRecord('token', {
        name: '',
        type: 'client',
        global: false,
      }),
      policies: this.store.peekAll('policy'),
      roles: this.store.peekAll('role'),
    };
  }

  resetController(controller, isExiting) {
    if (isExiting) {
      // Discard the draft record if the user leaves without saving
      const { token } = controller.model;
      if (token.isNew) {
        token.destroyRecord();
      }
    }
  }
}
