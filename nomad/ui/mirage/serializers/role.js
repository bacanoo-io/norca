/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import ApplicationSerializer from './application';

// The ACL role API returns the role's policies as a list of policy links,
// e.g. "Policies": [{ "Name": "readonly" }]. Mirage stores them as a
// relationship to policy models (whose IDs are their names).
export default ApplicationSerializer.extend({
  serializeIds: 'always',

  keyForRelationshipIds(relationship) {
    if (relationship === 'policies') {
      return 'Policies';
    }
    return ApplicationSerializer.prototype.keyForRelationshipIds.apply(
      this,
      arguments
    );
  },

  serialize() {
    const json = ApplicationSerializer.prototype.serialize.apply(
      this,
      arguments
    );
    if (Array.isArray(json)) {
      json.forEach(toPolicyLinks);
    } else if (json) {
      toPolicyLinks(json);
    }
    return json;
  },
});

function toPolicyLinks(role) {
  role.Policies = (role.Policies || []).map((name) => ({ Name: name }));
  return role;
}
