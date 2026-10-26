/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Factory } from 'miragejs';

export default Factory.extend({
  name: (i) => `node-pool-${i}`,
  description: (i) => `describe node-pool-${i}`,
  meta: {},
  schedulerConfiguration: {},
});
