/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, hasMany, belongsTo } from 'miragejs';

export default Model.extend({
  taskGroups: hasMany('task-group'),
  jobSummary: belongsTo('job-summary'),
  jobScale: belongsTo('job-scale'),
});
