/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { click, find, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import faker from 'nomad-ui/mirage/faker';
import { allScenarios } from '../../mirage/scenarios/default';

/**
 * Common setup of the Access Control modules: an application test with
 * mirage, the roles cluster scenario, and (unless `signIn` is false) a
 * management token signed in before every test.
 */
export function setupAcl(hooks, { signIn = true } = {}) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    faker.seed(1);
    allScenarios.rolesTestCluster(this.server);
    if (signIn) {
      signInWith(this.server, { type: 'management' });
    }
  });

  hooks.afterEach(function () {
    window.localStorage.clear();
  });
}

/**
 * A small driver for the Access Control acceptance tests. It speaks in
 * terms of the section (record types, row names, the requests that reached
 * mirage) so the tests read as user flows rather than as selectors.
 */

/**
 * Signs in with the first mirage token that has all the given attributes,
 * e.g. { type: 'management' } or { name: 'Multi Role Token' }.
 */
export function signInWith(server, attributes) {
  const token = server.db.tokens.findBy(attributes);
  if (!token) {
    throw new Error(`no mirage token matches ${JSON.stringify(attributes)}`);
  }
  window.localStorage.nomadTokenSecret = token.secretId;
  return token;
}

/**
 * Opens a page of the section: open() is the overview, open('tokens') the
 * token list, open('roles', 'editor') the page of role "editor".
 */
export function open(...segments) {
  return visit(['/access-control', ...segments].join('/'));
}

// The list rows of a record type (token, role, policy).
export function rowsOf(type) {
  return findAll(`[data-test-${type}-row]`);
}

// The text of one cell of a row, by the cell's field name.
export function cellText(row, type, field) {
  return row.querySelector(`[data-test-${type}-${field}]`)?.textContent.trim();
}

// The row of a record type whose name cell reads exactly `name`.
export function rowNamed(type, name) {
  return rowsOf(type).find((row) => cellText(row, type, 'name') === name);
}

// The names shown in the rows of a record type, top to bottom.
export function namesOf(type) {
  return rowsOf(type).map((row) => cellText(row, type, 'name'));
}

/**
 * Ticks the checkbox of the row named `name` in a picker table (the policy
 * or role choosers of the editors). `scope` selects the table, `nameField`
 * the cell holding the name.
 */
export async function pick(scope, nameField, name) {
  const row = findAll(`${scope} tbody tr`).find(
    (tr) => tr.querySelector(nameField)?.textContent.trim() === name
  );
  if (!row) {
    throw new Error(`no row "${name}" in ${scope}`);
  }
  await click(row.querySelector('input'));
}

// Names of the rows of a picker table whose checkbox is ticked, sorted.
export function picked(scope, nameField) {
  return findAll(`${scope} tbody tr input:checked`)
    .map((input) =>
      input.closest('tr').querySelector(nameField).textContent.trim()
    )
    .sort();
}

/**
 * Runs a two-step delete button (idle, then confirm) inside `container`,
 * which is a selector or an element.
 */
export async function confirmDelete(container) {
  const root = typeof container === 'string' ? find(container) : container;
  await click(root.querySelector('[data-test-idle-button]'));
  await click(root.querySelector('[data-test-confirm-button]'));
}

/**
 * The requests mirage handled with the given method whose URL matches
 * `url` (a string the URL ends with, or a RegExp), oldest first.
 */
export function requestsTo(server, method, url) {
  const matches =
    typeof url === 'string' ? (u) => u.endsWith(url) : (u) => url.test(u);
  return server.pretender.handledRequests.filter(
    (request) => request.method === method && matches(request.url)
  );
}

// The JSON body of the latest such request, or undefined.
export function lastBodyTo(server, method, url) {
  const request = requestsTo(server, method, url).pop();
  return request ? JSON.parse(request.requestBody) : undefined;
}

// Mirage tokens linked to the role with the given ID.
export function tokensWithRole(server, roleId) {
  return server.db.tokens.filter((token) =>
    (token.roleIds || []).includes(roleId)
  );
}
