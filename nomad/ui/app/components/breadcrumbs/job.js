/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { action } from '@ember/object';
import BreadcrumbsTemplate from './default';

export default class BreadcrumbsJob extends BreadcrumbsTemplate {
  get job() {
    return this.args.crumb.job;
  }

  get hasParent() {
    return !!this.job.belongsTo('parent').id();
  }

  // The parent job can disappear while it loads (switching regions unloads
  // the store); the breadcrumb then simply shows no parent.
  @action
  onError(err) {
    // eslint-disable-next-line no-console
    console.warn('Could not load the parent job', err?.Error ?? err);
  }

  @action
  fetchParent() {
    if (this.hasParent) {
      return this.job.get('parent');
    }
  }
}
