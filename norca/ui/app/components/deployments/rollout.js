/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsNamespace } from 'nomad-ui/utils/norca-acl';
import {
  multiregionRollout,
  deploymentAction,
  needsPromotion,
  ON_FAILURE,
} from 'nomad-ui/utils/deployments';

const LABELS = {
  run: 'Start',
  unblock: 'Unblock',
  promote: 'Promote canaries',
  pause: 'Pause',
  resume: 'Resume',
  fail: 'Mark as failed',
};

/**
 * The per-region timeline of a multiregion deployment (job, namespace,
 * version): one step per region in rollout order with its status and
 * progress, and the actions of that region's deployment: start a pending
 * one, unblock a blocked one, promote canaries, pause, resume, fail.
 * Actions need submit-job and go to the region of the deployment.
 */
export default class DeploymentsRollout extends Component {
  @service token;
  @service system;
  @service eventStream;

  @tracked rollout = null;
  @tracked isLoading = true;
  @tracked error = null;
  @tracked working = null;
  @tracked actionError = null;
  @tracked confirming = null;

  get canAct() {
    return allowsNamespace(this.token, this.args.namespace, 'submit-job');
  }

  get strategyText() {
    const s = this.rollout?.strategy || {};
    const parallel = s.MaxParallel
      ? `${s.MaxParallel} ${
          s.MaxParallel === 1 ? 'region' : 'regions'
        } at a time`
      : 'all regions at once';
    return `${parallel}; on failure ${
      ON_FAILURE[s.OnFailure || ''] || s.OnFailure
    }`;
  }

  get steps() {
    const all = (this.system.regions || []).slice().sort();
    return (this.rollout?.steps || []).map((step) => {
      const d = step.deployment;
      const status = d?.Status;
      const actions = [];
      if (d && this.canAct) {
        if (status === 'pending') actions.push('run');
        if (status === 'blocked' || status === 'unblocking')
          actions.push('unblock');
        if (status === 'running' && needsPromotion(d)) actions.push('promote');
        if (status === 'running') actions.push('pause');
        if (status === 'paused') actions.push('resume');
        if (['running', 'paused', 'pending', 'blocked'].includes(status))
          actions.push('fail');
      }
      const position = all.indexOf(step.region);
      return {
        ...step,
        colorClass: `norca-region-${
          ((position < 0 ? step.index : position) % 6) + 1
        }`,
        status: d
          ? status
          : step.state === 'superseded'
          ? 'superseded'
          : 'not started',
        description:
          d?.StatusDescription ||
          (step.state === 'superseded'
            ? 'A newer version is deploying in this region.'
            : step.error
            ? `The region is ${step.error}.`
            : 'No deployment of this version yet.'),
        actions: actions.map((a) => ({
          id: a,
          label: LABELS[a],
          variant:
            a === 'fail'
              ? 'outline-danger'
              : a === 'run' || a === 'unblock'
              ? 'primary'
              : 'secondary',
        })),
      };
    });
  }

  async load() {
    try {
      this.rollout = await multiregionRollout(this.token, {
        job: this.args.job,
        namespace: this.args.namespace,
        version: this.args.version,
        homeRegion: this.args.homeRegion,
      });
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [job, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const regions = this.system.regions?.length
      ? this.system.regions
      : [this.args.homeRegion || ''];
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: [`Deployment`],
        region,
        namespace,
        onEvents: (events) => {
          if (events.some((e) => e.Payload?.Deployment?.JobID === job))
            waitForPromise(this.load());
        },
        onResync: () => this.load(),
      })
    );
    return () => unsubscribers.forEach((u) => u());
  });

  @action
  ask(step, act) {
    this.actionError = null;
    if (act.id === 'fail' || act.id === 'unblock') {
      this.confirming = { step, act };
      return;
    }
    return this.run(step, act);
  }

  @action
  cancel() {
    this.confirming = null;
  }

  @action
  confirm() {
    const { step, act } = this.confirming;
    return this.run(step, act);
  }

  async run(step, act) {
    const d = step.deployment;
    this.working = `${step.region}:${act.id}`;
    try {
      let name = act.id;
      let extra = {};
      if (act.id === 'pause') extra = { Pause: true };
      if (act.id === 'resume') {
        name = 'pause';
        extra = { Pause: false };
      }
      if (act.id === 'promote') extra = { All: true };
      await deploymentAction(this.token, step.region, name, d.ID, extra);
      this.confirming = null;
      await this.load();
      this.args.onChange?.();
    } catch (e) {
      this.actionError = `${act.label} in ${step.region}: ${errorMessage(e)}`;
    } finally {
      this.working = null;
    }
  }
}
