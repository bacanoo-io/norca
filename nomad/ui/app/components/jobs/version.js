/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';
import messageForError from 'nomad-ui/utils/message-from-adapter-error';

const CHANGE_TYPES = ['Added', 'Deleted', 'Edited'];

const countChanges = (total, field) =>
  CHANGE_TYPES.includes(field.Type) ? total + 1 : total;

function fieldChanges(diff) {
  return (
    (diff.Fields || []).reduce(countChanges, 0) +
    (diff.Objects || []).reduce(arrayOfFieldChanges, 0)
  );
}

function arrayOfFieldChanges(count, diff) {
  return diff ? count + fieldChanges(diff) : count;
}

/**
 * A version of a job (Versions tab). Adapted from the job-version
 * component of the previous UI.
 */
export default class JobsVersion extends Component {
  @service router;

  @tracked isOpen = false;

  get changeCount() {
    const diff = this.args.version.diff;
    if (!diff) return 0;
    const taskGroups = diff.TaskGroups || [];
    const tasks = taskGroups.flatMap((tg) => tg.Tasks || []);
    return (
      fieldChanges(diff) +
      taskGroups.reduce(arrayOfFieldChanges, 0) +
      tasks.reduce(arrayOfFieldChanges, 0)
    );
  }

  get isCurrent() {
    const version = this.args.version;
    return version.number === version.get('job.version');
  }

  @action
  toggleDiff() {
    this.isOpen = !this.isOpen;
  }

  @task({ drop: true })
  *revertTo() {
    const version = this.args.version;
    try {
      const before = version.get('job.version');
      yield version.revertTo();
      yield version.job.reload();
      const after = version.get('job.version');
      if (before === after) {
        this.args.handleError?.({
          level: 'warn',
          title: 'Reversion Had No Effect',
          description:
            'Reverting to an identical older version doesn’t produce a new version',
        });
      } else {
        const job = version.get('job');
        this.router.transitionTo('jobs.job.index', job.get('idWithNamespace'));
      }
    } catch (e) {
      this.args.handleError?.({
        level: 'danger',
        title: 'Could Not Revert',
        description: messageForError(e, 'revert'),
      });
    }
  }
}
