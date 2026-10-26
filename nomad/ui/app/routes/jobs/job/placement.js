/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

export default class PlacementRoute extends Route {
  model() {
    return this.modelFor('jobs.job');
  }
}
