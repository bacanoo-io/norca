/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

// The releases of the regions in scope.
export default class PackagesIndexRoute extends Route {
  buildRouteInfoMetadata() {
    return { spansRegions: true };
  }
}
