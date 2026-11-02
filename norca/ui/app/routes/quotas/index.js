/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

// The quotas of the regions in scope (one region or all regions).
export default class QuotasIndexRoute extends Route {
  buildRouteInfoMetadata() {
    return { spansRegions: true };
  }
}
