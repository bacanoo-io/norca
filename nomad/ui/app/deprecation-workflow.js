/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import setupDeprecationWorkflow from 'ember-cli-deprecation-workflow';

setupDeprecationWorkflow({
  workflow: [
    { handler: 'throw', matchId: 'ember-inflector.globals' },
    { handler: 'throw', matchId: 'ember-runtime.deprecate-copy-copyable' },
    { handler: 'throw', matchId: 'ember-console.deprecate-logger' },
    {
      handler: 'throw',
      matchId: 'ember-test-helpers.rendering-context.jquery-element',
    },
    { handler: 'throw', matchId: 'ember-cli-page-object.is-property' },
    { handler: 'throw', matchId: 'ember-views.partial' },

    // Known deprecations of the code inherited from the Nomad 1.6 UI,
    // removed in Ember 7 / Ember Data 5. They are resolved as the pages are
    // migrated to the new component layer (UI plan, phases 2 to 4).
    { handler: 'silence', matchId: /^deprecate-import-.*-from-ember$/ },
    { handler: 'silence', matchId: 'importing-inject-from-ember-service' },
    { handler: 'silence', matchId: 'using-amd-bundles' },
    {
      handler: 'silence',
      matchId: 'ember-data:deprecate-non-strict-relationships',
    },
    { handler: 'silence', matchId: 'ember-data:deprecate-array-like' },
    { handler: 'silence', matchId: 'ember-data:deprecate-promise-proxies' },
    {
      handler: 'silence',
      matchId: 'ember-data:deprecate-many-array-duplicates',
    },
    { handler: 'silence', matchId: 'ember-data:deprecate-non-strict-types' },
    { handler: 'silence', matchId: 'ember-data:deprecate-store-find' },
    {
      handler: 'silence',
      matchId: 'ember-concurrency.deprecate-classic-task-api',
    },
    {
      handler: 'silence',
      matchId: 'ember-concurrency.deprecate-decorator-task',
    },
    { handler: 'silence', matchId: 'ember-modifier.use-modify' },
  ],
});
