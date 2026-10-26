/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
const EmberApp = require('ember-cli/lib/broccoli/ember-app');

const environment = EmberApp.env();
const isTest = environment === 'test';

module.exports = function (defaults) {
  let app = new EmberApp(defaults, {
    babel: {
      // dynamic import() of npm packages (xterm, ...)
      plugins: [require.resolve('ember-auto-import/babel-plugin')],
    },
    hinting: isTest,
    tests: isTest,
    sassOptions: {
      precision: 4,
      includePaths: ['./node_modules'],
      quietDeps: true,
      silenceDeprecations: [
        'import',
        'global-builtin',
        'color-functions',
        'if-function',
        'slash-div',
        'mixed-decls',
        'function-units',
      ],
    },
  });

  // Use `app.import` to add additional libraries to the generated
  // output files.
  //
  // If you need to use different assets in different
  // environments, specify an object as the first parameter. That
  // object's keys should be the environment name and the values
  // should be the asset to use in that environment.
  //
  // If the library that you are including contains AMD or ES6
  // modules that you would like to import into your application
  // please specify an object with the list of modules as keys
  // along with the exports of each module as its value.

  app.import('node_modules/xterm/css/xterm.css');
  app.import('node_modules/jsonlint/lib/jsonlint.js');
  app.import('node_modules/codemirror/addon/lint/lint.css');
  app.import('node_modules/codemirror/lib/codemirror.css');

  return app.toTree();
};
