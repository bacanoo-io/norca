/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, belongsTo } from 'miragejs';

export default Model.extend({
  allocation: belongsTo(),
});
