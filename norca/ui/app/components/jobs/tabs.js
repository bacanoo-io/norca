/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

/**
 * The tabs of the job page (UI plan, job details): Overview, Allocations,
 * Deployments, Versions, Scaling, Services, Placement, Events, then the
 * Evaluations, Clients (system and sysbatch jobs), Definition and
 * Variables of the previous job page.
 *
 * The parents of periodic and parameterized jobs have no allocations of
 * their own: they show only the tabs that make sense for them.
 */
export default class JobsTabs extends Component {
  @service can;

  get tabs() {
    const job = this.args.job;
    const model = job;
    const isParent = job.hasChildren;
    const tabs = [
      {
        id: 'overview',
        label: 'Overview',
        route: 'jobs.job.index',
        model,
        currentWhen: 'jobs.job.index jobs.job.dispatch',
      },
    ];
    if (!isParent) {
      tabs.push({
        id: 'allocations',
        label: 'Allocations',
        route: 'jobs.job.allocations',
        model,
      });
    }
    if (job.supportsDeployments) {
      tabs.push({
        id: 'deployments',
        label: 'Deployments',
        route: 'jobs.job.deployments',
        model,
      });
    }
    tabs.push({
      id: 'versions',
      label: 'Versions',
      route: 'jobs.job.versions',
      model,
    });
    if (!isParent && job.type !== 'system' && job.type !== 'sysbatch') {
      tabs.push({
        id: 'scaling',
        label: 'Scaling',
        route: 'jobs.job.scaling',
        model,
      });
    }
    if (!isParent) {
      tabs.push({
        id: 'services',
        label: 'Services',
        route: 'jobs.job.services',
        model,
        currentWhen: 'jobs.job.services',
      });
      tabs.push({
        id: 'placement',
        label: 'Placement',
        route: 'jobs.job.placement',
        model,
      });
    }
    tabs.push({
      id: 'events',
      label: 'Events',
      route: 'jobs.job.events',
      model,
    });
    if (!isParent) {
      tabs.push({
        id: 'evaluations',
        label: 'Evaluations',
        route: 'jobs.job.evaluations',
        model,
      });
      if (job.hasClientStatus && this.can.can('read client')) {
        tabs.push({
          id: 'clients',
          label: 'Clients',
          route: 'jobs.job.clients',
          model,
        });
      }
    }
    tabs.push({
      id: 'definition',
      label: 'Definition',
      route: 'jobs.job.definition',
      model,
    });
    if (this.can.can('list variables')) {
      tabs.push({
        id: 'variables',
        label: 'Variables',
        route: 'jobs.job.variables',
        model,
      });
    }
    return tabs;
  }
}
