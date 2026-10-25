/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, belongsTo, hasMany } from 'miragejs';

export default Model.extend({
  taskGroup: belongsTo(),
  services: hasMany('service-fragment'),
});
