/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Model, hasMany } from 'miragejs';

// A token names policies and roles; neither of them knows its tokens, so
// both relations are one-way.
const oneWay = (type) => hasMany(type, { inverse: null });

export default Model.extend({
  policies: oneWay('policy'),
  roles: oneWay('role'),
});
