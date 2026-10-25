/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember/no-controller-access-in-routes */
import Route from '@ember/routing/route';
import legacyTransitionTo from 'nomad-ui/utils/legacy-transition';

export default class OptimizeIndexRoute extends Route {
  async activate() {
    // This runs late in the loading lifecycle to ensure .filteredSummaries is populated
    const summaries = this.controllerFor('optimize').filteredSummaries;

    if (summaries.length) {
      const firstSummary = summaries.objectAt(0);

      return legacyTransitionTo(this, 'optimize.summary', firstSummary.slug, {
        queryParams: {
          jobNamespace: firstSummary.jobNamespace || 'default',
        },
      });
    }
  }
}
