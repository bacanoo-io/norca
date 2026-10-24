/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import { hash } from 'rsvp';
import notifyError from 'nomad-ui/utils/notify-error';

export default class AccessControlTokensTokenRoute extends Route {
  @service store;

  model(params) {
    // The token list only holds stubs; fetch the full token (including its
    // secret) every time the page is opened.
    return hash({
      token: this.store.findRecord('token', params.id, { reload: true }),
      policies: this.store.peekAll('policy'),
      roles: this.store.peekAll('role'),
    }).catch(notifyError(this));
  }

  resetController(controller, isExiting) {
    super.resetController(...arguments);
    if (isExiting) {
      // Drop unsaved edits to the name and type
      const token = controller.model?.token;
      if (token && !token.isDeleted && token.hasDirtyAttributes) {
        token.rollbackAttributes();
      }
    }
  }
}
