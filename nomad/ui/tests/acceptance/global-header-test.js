/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable ember-a11y-testing/a11y-audit-called */
import { module, test } from 'qunit';
import { click, visit, currentURL } from '@ember/test-helpers';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import Layout from 'nomad-ui/tests/pages/layout';

let managementToken;

// The top bar of the global frame: external UI links and the previous UI
// live in the "More" menu (or the user menu when signed in).
module('Acceptance | global header', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
  });

  test('it displays no external links', async function (assert) {
    server.create('agent');

    await visit('/');
    await Layout.navbar.end.moreMenu.open();

    assert.false(Layout.navbar.end.consulLink.isPresent);
    assert.false(Layout.navbar.end.vaultLink.isPresent);
    assert
      .dom('[data-test-legacy-ui-link]')
      .hasAttribute('href', '/ui/legacy/');
  });

  test('it displays the Consul and Vault links', async function (assert) {
    server.create('agent', 'withConsulLink', 'withVaultLink');

    await visit('/');
    await Layout.navbar.end.moreMenu.open();

    assert.equal(Layout.navbar.end.consulLink.text, 'Consul');
    assert.equal(Layout.navbar.end.consulLink.link, 'http://localhost:8500/ui');
    assert.equal(Layout.navbar.end.vaultLink.text, 'Vault');
    assert.equal(Layout.navbar.end.vaultLink.link, 'http://localhost:8200/ui');
  });

  test('it displays Sign in without a token', async function (assert) {
    managementToken = server.create('token');

    await visit('/');
    assert.true(Layout.navbar.end.signInLink.isVisible);
    assert.false(Layout.navbar.end.profileDropdown.isPresent);
  });

  test('it displays the user menu with a token', async function (assert) {
    managementToken = server.create('token');
    window.localStorage.nomadTokenSecret = managementToken.secretId;

    await visit('/');
    assert.true(Layout.navbar.end.profileDropdown.isPresent);
    assert.false(Layout.navbar.end.signInLink.isPresent);
    assert.equal(Layout.navbar.end.profileDropdown.text, managementToken.name);

    await Layout.navbar.end.profileDropdown.open();
    await click('[data-test-profile-dropdown-profile-link]');
    assert.equal(
      currentURL(),
      '/settings/tokens',
      'Profile goes to the tokens page'
    );

    await Layout.navbar.end.profileDropdown.open();
    await click('[data-test-profile-dropdown-sign-out-link]');
    assert.equal(window.localStorage.nomadTokenSecret, null, 'Token is wiped');
    assert.equal(currentURL(), '/jobs', 'After signout, back on the jobs page');
  });

  test('the theme toggle switches between light and dark mode', async function (assert) {
    await visit('/');
    const root = document.documentElement;
    const before = root.getAttribute('data-bs-theme');
    await Layout.navbar.themeToggle();
    assert.notEqual(root.getAttribute('data-bs-theme'), before);
    await Layout.navbar.themeToggle();
    assert.equal(root.getAttribute('data-bs-theme'), before);
  });
});
