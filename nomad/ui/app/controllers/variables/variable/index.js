/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller, { inject as controller } from '@ember/controller';
import { set, action } from '@ember/object';
import { task } from 'ember-concurrency';
import { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';

export default class VariablesVariableIndexController extends Controller {
  queryParams = ['view'];

  @service router;
  queryParams = ['sortProperty', 'sortDescending'];

  @tracked sortProperty = 'key';
  @tracked sortDescending = true;

  @service notifications;

  get sortedKeyValues() {
    const sorted = this.model.keyValues.sortBy(this.sortProperty);
    return this.sortDescending ? sorted : sorted.reverse();
  }

  @controller('variables.variable') variableController;

  itemColumns = [
    { key: 'key', label: 'Key', sortable: true },
    { key: 'value', label: 'Value', sortable: true },
  ];

  @action
  sort(sortProperty, sortDescending) {
    this.sortProperty = sortProperty;
    this.sortDescending = sortDescending;
  }

  @tracked isDeleting = false;

  @action
  onDeletePrompt() {
    this.isDeleting = true;
  }

  @action
  onDeleteCancel() {
    this.isDeleting = false;
  }

  @task(function* () {
    try {
      yield this.model.deleteRecord();
      yield this.model.save();
      if (this.model.parentFolderPath) {
        this.router.transitionTo('variables.path', this.model.parentFolderPath);
      } else {
        this.router.transitionTo('variables');
      }
      this.notifications.add({
        title: 'Variable deleted',
        message: `${this.model.path} successfully deleted`,
        color: 'success',
      });
    } catch (err) {
      this.notifications.add({
        title: `Error deleting ${this.model.path}`,
        message: err,
        color: 'critical',
        sticky: true,
      });
    }
  })
  deleteVariableFile;

  //#region Code View
  /**
   * @type {"table" | "json"}
   */
  @tracked
  view = 'table';

  @action
  toggleView() {
    if (this.view === 'table') {
      this.view = 'json';
    } else {
      this.view = 'table';
    }
  }
  //#endregion Code View

  get shouldShowLinkedEntities() {
    return (
      this.model.pathLinkedEntities?.job ||
      this.model.pathLinkedEntities?.group ||
      this.model.pathLinkedEntities?.task ||
      this.model.path === 'nomad/jobs'
    );
  }

  @action
  toggleRowVisibility(kv) {
    set(kv, 'isVisible', !kv.isVisible);
  }
}
