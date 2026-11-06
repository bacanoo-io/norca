/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { action } from '@ember/object';
import config from 'nomad-ui/config/environment';
import TokensController from 'nomad-ui/controllers/settings/tokens';

/**
 * The sign-in page: the sign-in of the tokens page (a token secret, a JWT or
 * an SSO provider), and after it the page asked for, or the dashboard.
 */
export default class LoginController extends TokensController {
  logoURL = `${config.rootURL}images/norca-logo.png`;

  async afterSignIn() {
    if (!(await super.afterSignIn()) && this.token.selfToken) {
      this.router.transitionTo('index');
    }
    return true;
  }

  // The identity provider sends the browser back to the tokens page, the
  // redirect URI SSO auth methods already allow; it opens the page asked
  // for, or the dashboard.
  ssoRedirectURL() {
    return new URL(
      this.router.urlFor('settings.tokens'),
      window.location.origin
    ).href;
  }

  @action redirectToSSO(method) {
    if (!this.token.loginRedirect) this.token.loginRedirect = '/';
    super.redirectToSSO(method);
  }
}
