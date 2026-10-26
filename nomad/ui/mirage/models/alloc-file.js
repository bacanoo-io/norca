/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, belongsTo } from 'miragejs';

export default Model.extend({
  parent: belongsTo('alloc-file'),
});
