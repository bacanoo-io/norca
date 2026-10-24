/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import notifyError from 'nomad-ui/utils/notify-error';

export default class AccessControlRolesRoleRoute extends Route {
  @service store;

  model(params) {
    return hash({
      role: this.store.findRecord('role', params.id, { reload: true }),
      policies: this.store.peekAll('policy'),
      tokens: this.store.peekAll('token'),
    }).catch(notifyError(this));
  }

  resetController(controller, isExiting) {
    super.resetController(...arguments);
    if (isExiting) {
      // Drop unsaved edits to the name and description
      const role = controller.model?.role;
      if (role && !role.isDeleted && role.hasDirtyAttributes) {
        role.rollbackAttributes();
      }
    }
  }
}
