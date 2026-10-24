/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, hasMany } from 'ember-cli-mirage';

// A token links to policies directly and to roles, which in turn link to
// further policies. Neither side keeps a reference back to the token.
export default Model.extend({
  policies: hasMany('policy', { inverse: null }),
  roles: hasMany('role', { inverse: null }),
});
