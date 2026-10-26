/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember/no-controller-access-in-routes */
import Route from '@ember/routing/route';

export default class NotFoundRoute extends Route {
  model() {
    const err = new Error('Page not found');
    err.code = '404';
    this.controllerFor('application').set('error', err);
  }
}
