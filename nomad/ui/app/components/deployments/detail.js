/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getDeployment,
  deploymentAllocations,
  progressSegments,
} from 'nomad-ui/utils/deployments';
import { formatNs } from 'nomad-ui/utils/scaling';

/**
 * A deployment: its task groups (desired, placed, healthy, canaries and
 * promotion), its allocations and, for a multiregion deployment, the
 * rollout over the regions with the actions.
 */
export default class DeploymentsDetail extends Component {
  @service token;
  @service system;
  @service eventStream;

  @tracked deployment = null;
  @tracked allocations = [];
  @tracked isLoading = true;
  @tracked loadError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get jobRouteId() {
    const d = this.deployment;
    return d ? `${d.JobID}@${d.Namespace || 'default'}` : '';
  }

  get progress() {
    return this.deployment ? progressSegments(this.deployment) : null;
  }

  get groups() {
    return Object.entries(this.deployment?.TaskGroups || {}).map(
      ([name, g]) => ({
        name,
        ...g,
        canaries: g.DesiredCanaries
          ? `${(g.PlacedCanaries || []).length} of ${g.DesiredCanaries}${
              g.Promoted ? ', promoted' : ''
            }`
          : '–',
        deadline: g.ProgressDeadline ? formatNs(g.ProgressDeadline) : '–',
      })
    );
  }

  get allocationRows() {
    return this.allocations.map((a) => ({
      ...a,
      shortId: a.ID.split('-')[0],
      health:
        a.DeploymentStatus?.Healthy === true
          ? 'healthy'
          : a.DeploymentStatus?.Healthy === false
          ? 'unhealthy'
          : 'pending',
      canary: !!a.DeploymentStatus?.Canary,
    }));
  }

  async load() {
    try {
      const { body } = await getDeployment(
        this.token,
        this.region,
        this.args.deploymentId
      );
      this.deployment = body;
      try {
        this.allocations =
          (
            await deploymentAllocations(
              this.token,
              this.region,
              this.args.deploymentId
            )
          ).body || [];
      } catch (e) {
        this.allocations = [];
      }
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The deployment does not exist in this region: deployments are kept per region and removed by garbage collection.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [id, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`Deployment:${id}`],
      region,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  refresh = () => this.load();
}
