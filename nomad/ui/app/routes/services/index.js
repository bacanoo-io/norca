/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

// The service catalog of the regions in scope.
export default class ServicesIndexRoute extends Route {
  buildRouteInfoMetadata() {
    return { spansRegions: true };
  }
}
