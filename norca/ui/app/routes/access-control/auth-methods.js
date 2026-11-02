/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';
import { inject as service } from '@ember/service';
import notifyError from 'nomad-ui/utils/notify-error';

/**
 * The auth methods of the cluster (single sign-on with OIDC or JWT). The
 * list needs no ACL permission; they are managed with the CLI.
 */
export default class AccessControlAuthMethodsRoute extends Route {
  @service store;

  model() {
    return this.store
      .findAll('auth-method', { reload: true })
      .catch(notifyError(this));
  }
}
