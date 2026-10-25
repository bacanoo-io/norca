/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, hasMany, belongsTo } from 'miragejs';

export default Model.extend({
  relatedEvals: hasMany('evaluation-stub'),
});
