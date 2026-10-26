/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

// Build-time style plumbing: Tabler's CSS (MIT) is global, but wrapped into
// the `tabler` cascade layer. Layered rules lose to unlayered ones, so the
// Norca styles (assets/nomad-ui.css) win wherever both style the same
// element, whatever the specificity.

const path = require('path');
const fs = require('fs');
const Plugin = require('broccoli-plugin');
const Funnel = require('broccoli-funnel');
const { wrapInLayer, LAYER_ORDER } = require('./scope');

class TablerLayer extends Plugin {
  constructor(input) {
    super([input], {
      annotation: 'norca-styles: tabler layer',
      persistentOutput: false,
    });
  }

  build() {
    const source = fs.readFileSync(
      path.join(this.inputPaths[0], 'tabler.css'),
      'utf8'
    );
    fs.mkdirSync(path.join(this.outputPath, 'norca'), { recursive: true });
    fs.writeFileSync(
      path.join(this.outputPath, 'norca', 'tabler.layered.css'),
      `${LAYER_ORDER}\n${wrapInLayer(source, 'tabler')}`
    );
  }
}

module.exports = {
  name: 'norca-styles',

  isDevelopingAddon() {
    return true;
  },

  included(app) {
    this._super.included.apply(this, arguments);
    while (typeof app.import !== 'function' && app.app) {
      app = app.app;
    }
    app.import('vendor/norca/tabler.layered.css');
  },

  treeForVendor() {
    const tablerCSS = path.join(
      path.dirname(require.resolve('@tabler/core/package.json')),
      'dist',
      'css'
    );
    return new TablerLayer(
      new Funnel(tablerCSS, {
        files: ['tabler.css'],
        annotation: 'Funnel (tabler css)',
      })
    );
  },
};
