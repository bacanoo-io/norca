/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import ApplicationSerializer from './application';

export default ApplicationSerializer.extend({
  keyForAttribute(str) {
    if (['config', 'member', 'norca_version', 'nomad_version'].includes(str)) {
      return str;
    }
    return ApplicationSerializer.prototype.keyForAttribute.apply(this, arguments);
  },
});
