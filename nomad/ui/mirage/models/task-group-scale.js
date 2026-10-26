/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, belongsTo, hasMany } from 'miragejs';

export default Model.extend({
  jobScale: belongsTo(),
  events: hasMany('scale-event'),
});
