/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import ApplicationSerializer from './application';

const base = ApplicationSerializer.prototype;

// Relationship ID keys that differ from the application default.
const ID_KEYS = { policies: 'Policies' };

// Mirage relates a role to policy models whose IDs are the policy names;
// the API lists them as links: [{ "Name": "readonly" }, ...].
const asPolicyLinks = (names) => (names || []).map((Name) => ({ Name }));

export default ApplicationSerializer.extend({
  serializeIds: 'always',

  keyForRelationshipIds(relationship) {
    return (
      ID_KEYS[relationship] ?? base.keyForRelationshipIds.apply(this, arguments)
    );
  },

  serialize() {
    const json = base.serialize.apply(this, arguments);
    // One role or a list of roles, depending on the request.
    for (const role of [].concat(json ?? [])) {
      role.Policies = asPolicyLinks(role.Policies);
    }
    return json;
  },
});
