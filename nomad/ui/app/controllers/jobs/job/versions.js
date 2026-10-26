/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import WithNamespaceResetting from 'nomad-ui/mixins/with-namespace-resetting';
import { alias } from '@ember/object/computed';
import { action, computed } from '@ember/object';
import classic from 'ember-classic-decorator';
import moment from 'moment';

const alertClassFallback = 'is-info';

const errorLevelToAlertClass = {
  danger: 'is-danger',
  warn: 'is-warning',
};

@classic
export default class VersionsController extends Controller.extend(
  WithNamespaceResetting
) {
  error = null;

  @alias('model') job;

  @computed('error.level')
  get errorLevelClass() {
    return (
      errorLevelToAlertClass[this.get('error.level')] || alertClassFallback
    );
  }

  // Newest first, with the date shown when the day changes.
  @computed('model.versions.@each.submitTime')
  get annotatedVersions() {
    const versions = (this.get('model.versions') || [])
      .toArray()
      .sortBy('submitTime')
      .reverse();
    return versions.map((version, index) => {
      let showDate = index === 0;
      if (!showDate) {
        const previous = moment(versions[index - 1].submitTime).startOf('day');
        const current = moment(version.submitTime).startOf('day');
        showDate = previous.diff(current, 'days') > 0;
      }
      return { version, showDate };
    });
  }

  onDismiss() {
    this.set('error', null);
  }

  @action
  handleError(errorObject) {
    this.set('error', errorObject);
  }
}
