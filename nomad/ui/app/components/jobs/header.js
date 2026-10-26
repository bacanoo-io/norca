/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';
import jsonToHcl from 'nomad-ui/utils/json-to-hcl';
import { jobOwner } from 'nomad-ui/utils/job-owner';

/**
 * The header of the job page (all tabs): breadcrumbs, the name and status,
 * the scope (region, namespace), the type and the owner (a release or an
 * operator object), and the actions: exec, force launch (periodic),
 * dispatch (parameterized), stop, start and purge.
 */
export default class JobsHeader extends Component {
  @service router;
  @service notifications;
  @service system;

  @tracked errorMessage = null;

  get breadcrumbs() {
    const job = this.args.job;
    const items = [{ label: 'Jobs', route: 'jobs.index', id: 'jobs.index' }];
    const parent = job.get?.('parent.content');
    if (parent) {
      items.push({
        label: parent.name,
        route: 'jobs.job.index',
        model: parent,
        id: 'jobs.job.parent',
      });
    }
    items.push({
      label: job.trimmedName || job.name,
      route: 'jobs.job.index',
      model: job,
      id: 'jobs.job.index',
    });
    return [...items, ...(this.args.trail || [])];
  }

  get isDispatchPage() {
    return this.router.currentRouteName === 'jobs.job.dispatch';
  }

  get region() {
    return this.system.shouldShowRegions
      ? this.args.job.region || this.system.activeRegion
      : null;
  }

  get owner() {
    return jobOwner(this.args.job);
  }

  @action
  clearError() {
    this.errorMessage = null;
  }

  handleError(error) {
    this.errorMessage = error;
    this.args.onError?.(error);
  }

  notify(title, message) {
    this.notifications.add({ title, message, color: 'success' });
  }

  /**
   * @param {boolean} withNotifications - show a toast on success, as when
   * triggered by the keyboard shortcut
   */
  @task({ drop: true })
  *stopJob(withNotifications = false) {
    const job = this.args.job;
    try {
      yield job.stop();
      // Eagerly update the job status to avoid flickering
      job.set('status', 'dead');
      if (withNotifications === true) {
        this.notify('Job Stopped', `${job.name} has been stopped`);
      }
    } catch (err) {
      this.handleError({
        title: 'Could Not Stop Job',
        description: messageFromAdapterError(err, 'stop jobs'),
      });
    }
  }

  @task({ drop: true })
  *purgeJob() {
    const job = this.args.job;
    try {
      yield job.purge();
      this.notify('Job Purged', `You have purged ${job.name}`);
      this.router.transitionTo('jobs');
    } catch (err) {
      this.handleError({
        title: 'Error purging job',
        description: messageFromAdapterError(err, 'purge jobs'),
      });
    }
  }

  @task({ drop: true })
  *startJob(withNotifications = false) {
    const job = this.args.job;

    // Try to get the submission/hcl sourced specification first. In the
    // event that this fails, fall back to the raw definition.
    try {
      const specification = yield job.fetchRawSpecification();
      let variables = job.get('_newDefinitionVariables') || '';
      if (specification.VariableFlags) {
        variables += jsonToHcl(specification.VariableFlags);
      }
      if (specification.Variables) {
        variables += specification.Variables;
      }
      job.set('_newDefinitionVariables', variables);
      job.set('_newDefinition', specification.Source);
    } catch {
      const definition = yield job.fetchRawDefinition();
      delete definition.Stop;
      job.set('_newDefinition', JSON.stringify(definition));
    }

    try {
      yield job.parse();
      yield job.update();
      // Eagerly update the job status to avoid flickering
      job.set('status', 'running');
      if (withNotifications === true) {
        this.notify('Job Started', `${job.name} has started`);
      }
    } catch (err) {
      this.handleError({
        title: 'Could Not Start Job',
        description: messageFromAdapterError(err, 'start jobs'),
      });
    }
  }

  @task({ drop: true })
  *forceLaunch() {
    try {
      yield this.args.job.forcePeriodic();
      this.notify(
        'Job Launched',
        `A new launch of ${this.args.job.name} was started`
      );
    } catch (err) {
      this.handleError({
        title: 'Could Not Force Launch',
        description: messageFromAdapterError(err, 'submit jobs'),
      });
    }
  }
}
