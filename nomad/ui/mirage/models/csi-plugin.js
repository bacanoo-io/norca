/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, hasMany } from 'miragejs';

export default Model.extend({
  nodes: hasMany('storage-node'),
  controllers: hasMany('storage-controller'),
});
