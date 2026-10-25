/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { getOwner } from '@ember/owner';

// Controller#transitionToRoute and Route#transitionTo were removed in
// Ember 5. The router service's transitionTo resets query params that are
// not passed, which re-runs the application route (its `region` query param
// refreshes the model) in the middle of the transition. The pages inherited
// from the Nomad 1.6 UI rely on the old behavior, which the application
// router still provides.
/* eslint-disable ember/no-private-routing-service */
export default function legacyTransitionTo(context, ...args) {
  const router = getOwner(context).lookup('router:main');
  return router.transitionTo(...args);
}
/* eslint-enable ember/no-private-routing-service */
