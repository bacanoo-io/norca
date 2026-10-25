/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import {
  attribute,
  create,
  clickable,
  collection,
  fillable,
  hasClass,
  isPresent,
  property,
  text,
  triggerable,
  value,
} from 'ember-cli-page-object';

export default create({
  navbar: {
    scope: '[data-test-global-header]',

    regionSwitcher: {
      scope: '[data-test-region-switcher-parent]',
      isPresent: isPresent(),
      value: value('select'),
      select: fillable('select'),
      options: collection('option', {
        label: text(),
        value: attribute('value'),
      }),
    },

    namespaceSwitcher: {
      scope: '[data-test-namespace-switcher]',
      isPresent: isPresent(),
      select: fillable(),
    },

    palette: {
      open: clickable('[data-test-palette-open]'),
    },

    themeToggle: clickable('[data-test-theme-toggle]'),

    notifications: {
      scope: '[data-test-notifications]',
      open: clickable('[data-test-dropdown-trigger]'),
      hasBadge: isPresent('[data-test-dropdown-badge]'),
      items: collection('[data-test-notification]', {
        text: text(),
      }),
    },

    end: {
      consulLink: {
        scope: '[data-test-header-consul-link]',
        testContainer: 'body',
        resetScope: true,
        text: text(),
        link: property('href'),
      },

      vaultLink: {
        scope: '[data-test-header-vault-link]',
        testContainer: 'body',
        resetScope: true,
        text: text(),
        link: property('href'),
      },

      moreMenu: {
        scope: '[data-test-header-more]',
        open: clickable('[data-test-dropdown-trigger]'),
      },

      signInLink: {
        scope: '[data-test-header-signin-link]',
        text: text(),
        link: property('href'),
      },

      profileDropdown: {
        scope: '[data-test-header-profile-dropdown]',
        text: text('[data-test-dropdown-trigger]'),
        open: clickable('[data-test-dropdown-trigger]'),
        options: collection('[data-test-dropdown-item]', {
          label: text(),
          choose: clickable(),
        }),
      },
    },
  },

  palette: {
    scope: '[data-test-palette]',
    testContainer: 'body',
    resetScope: true,
    isPresent: isPresent(),
    input: fillable('[data-test-palette-input]'),
    keydown: triggerable('keydown', '[data-test-palette-input]'),
    items: collection('[data-test-palette-item]', {
      id: attribute('data-test-palette-item'),
      label: text(),
      choose: clickable(),
      isActive: hasClass('active'),
    }),
    emptyMessage: text('[data-test-palette-empty]'),
  },

  gutter: {
    scope: '[data-test-gutter-menu]',
    visitDashboard: clickable('[data-test-gutter-link="dashboard"]'),
    visitJobs: clickable('[data-test-gutter-link="jobs"]'),

    variables: {
      scope: '[data-test-gutter-link="variables"]',
    },

    links: collection('[data-test-gutter-link]', {
      id: attribute('data-test-gutter-link'),
      label: text(),
    }),
    groups: collection('[data-test-nav-group]', {
      label: text(),
    }),

    toggleCollapse: clickable('[data-test-sidebar-collapse]'),

    visitClients: clickable('[data-test-gutter-link="clients"]'),
    visitServers: clickable('[data-test-gutter-link="servers"]'),
    visitStorage: clickable('[data-test-gutter-link="storage"]'),
  },

  breadcrumbs: collection('[data-test-breadcrumb]', {
    id: attribute('data-test-breadcrumb'),
    text: text(),
    visit: clickable(),
  }),

  breadcrumbFor(id) {
    return this.breadcrumbs.toArray().find((crumb) => crumb.id === id);
  },

  error: {
    isPresent: isPresent('[data-test-error]'),
    title: text('[data-test-error-title]'),
    message: text('[data-test-error-message]'),
  },

  inlineError: {
    isShown: isPresent('[data-test-inline-error]'),
    title: text('[data-test-inline-error-title]'),
    message: text('[data-test-inline-error-body]'),
    dismiss: clickable('[data-test-inline-error-close]'),

    isDanger: hasClass('is-danger', '[data-test-inline-error]'),
    isWarning: hasClass('is-warning', '[data-test-inline-error]'),
  },

  keyboard: {
    modalShown: isPresent('.keyboard-shortcuts'),
  },
});
