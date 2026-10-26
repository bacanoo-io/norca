/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Controller from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { next, schedule } from '@ember/runloop';

export default class JobController extends Controller {
  @service router;
  @service notifications;
  @service store;
  queryParams = [
    {
      jobNamespace: 'namespace',
    },
  ];
  jobNamespace = 'default';

  get job() {
    return this.model;
  }

  // The task group page has its own header, without the job's tabs.
  get isTaskGroupPage() {
    return this.router.currentRouteName === 'jobs.job.task-group';
  }

  // Called by {{did-update}} while rendering: the notification and the
  // transition happen after the render (Ember 6 forbids updating state that
  // the render already read).
  @action notFoundJobHandler() {
    if (
      this.watchers.job.isError &&
      this.watchers.job.error?.errors?.some((e) => e.status === '404')
    ) {
      schedule('actions', this, this.handleDeletedJob);
    }
  }

  handleDeletedJob() {
    if (this.isDestroying || this.isDestroyed) return;
    this.notifications.add({
      title: `Job "${this.job.name}" has been deleted`,
      message:
        'The job you were looking at has been deleted; this is usually because it was purged from elsewhere.',
      color: 'critical',
      sticky: true,
    });
    // Unload the job only after leaving its pages, which still render it.
    const job = this.job;
    this.router.transitionTo('jobs').finally(() => {
      next(() => {
        if (!job.isDestroyed && !job.isDestroying) this.store.unloadRecord(job);
      });
    });
  }
}
