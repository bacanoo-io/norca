/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/**
 * The three record types of the Access Control section, described once.
 * Routes, controllers and the acl-records service read these entries
 * instead of repeating the same knowledge per type.
 *
 * - noun:      singular word used in notifications ("Token x deleted")
 * - plural:    the ability subject for "list ..." and error messages
 * - section:   the route of the list page
 * - detail:    the route of a single record, and the attribute its URL uses
 * - draft:     attributes of a new, unsaved record
 * - related:   other collections a page needs next to its own records
 * - linkedBy:  record types whose hasMany(<plural>) has to forget a deleted
 *              record, so lists don't keep showing it
 */
const KINDS = {
  policy: {
    noun: 'Policy',
    plural: 'policies',
    section: 'access-control.policies',
    detail: { route: 'access-control.policies.policy', param: 'name' },
    linkedBy: ['role', 'token'],
  },
  role: {
    noun: 'Role',
    plural: 'roles',
    section: 'access-control.roles',
    detail: { route: 'access-control.roles.role', param: 'id' },
    draft: { name: '', description: '' },
    related: { policies: 'policy', tokens: 'token' },
    newRelated: { policies: 'policy' },
    linkedBy: ['token'],
  },
  token: {
    noun: 'Token',
    plural: 'tokens',
    section: 'access-control.tokens',
    detail: { route: 'access-control.tokens.token', param: 'id' },
    draft: { name: '', type: 'client', global: false },
    related: { policies: 'policy', roles: 'role' },
    newRelated: { policies: 'policy', roles: 'role' },
    linkedBy: [],
  },
};

/**
 * Returns the description of a record type; unknown types are a
 * programming error.
 */
export function kind(type) {
  const entry = KINDS[type];
  if (!entry) {
    throw new Error(`access-control: unknown record type ${type}`);
  }
  return { type, ...entry };
}

export default KINDS;
