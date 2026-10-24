/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';

export default class AccessControlRolesNewRoute extends Route {
  @service can;
  @service router;
  @service store;

  beforeModel() {
    if (this.can.cannot('write role')) {
      this.router.transitionTo('/access-control/roles');
    }
  }

  model() {
    return {
      role: this.store.createRecord('role', {
        name: '',
        description: '',
      }),
      policies: this.store.peekAll('policy'),
    };
  }

  resetController(controller, isExiting) {
    if (isExiting) {
      // Discard the draft record if the user leaves without saving
      const { role } = controller.model;
      if (role.isNew) {
        role.destroyRecord();
      }
    }
  }
}
