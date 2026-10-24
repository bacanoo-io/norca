/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  clickable,
  collection,
  create,
  isPresent,
  text,
  visitable,
} from 'ember-cli-page-object';

export default create({
  visit: visitable('/access-control'),
  visitTokens: visitable('/access-control/tokens'),
  visitRoles: visitable('/access-control/roles'),
  visitPolicies: visitable('/access-control/policies'),
  visitNewToken: visitable('/access-control/tokens/new'),
  visitNewRole: visitable('/access-control/roles/new'),
  visitToken: visitable('/access-control/tokens/:id'),
  visitRole: visitable('/access-control/roles/:id'),

  title: text('[data-test-title]'),

  subnav: {
    scope: '[data-test-access-control-subnav]',
    tabs: collection('[data-test-tab]', {
      name: text(),
      click: clickable('a'),
    }),
  },

  tokensCard: {
    scope: '[data-test-tokens-card]',
    count: text('[data-test-tokens-count]'),
  },
  rolesCard: {
    scope: '[data-test-roles-card]',
    count: text('[data-test-roles-count]'),
  },
  policiesCard: {
    scope: '[data-test-policies-card]',
    count: text('[data-test-policies-count]'),
  },

  createToken: clickable('[data-test-create-token]'),
  createRole: clickable('[data-test-create-role]'),
  createPolicy: clickable('[data-test-create-policy]'),

  tokens: collection('[data-test-token-row]', {
    name: text('[data-test-token-name]'),
    type: text('[data-test-token-type]'),
    expiration: text('[data-test-token-expiration-time]'),
    roles: text('[data-test-token-roles]'),
    policies: text('[data-test-token-policies]'),
    clickName: clickable('[data-test-token-name] a'),
    delete: clickable('[data-test-delete-token] button'),
  }),

  roles: collection('[data-test-role-row]', {
    name: text('[data-test-role-name]'),
    description: text('[data-test-role-description]'),
    policies: text('[data-test-role-policies]'),
    tokenCount: text('[data-test-role-total-tokens]'),
    clickName: clickable('[data-test-role-name] a'),
    delete: clickable('[data-test-delete-role]'),
  }),

  emptyRoles: isPresent('[data-test-empty-roles-list-headline]'),
});
