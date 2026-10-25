/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

// Build-time style plumbing of the Tabler migration (UI plan, "Tabler
// migration", step 4):
//
// - Tabler's CSS (MIT) is global, but wrapped into the `tabler` cascade
//   layer. Layered rules lose to unlayered ones, so the legacy stylesheet
//   wins wherever both style the same element.
// - The legacy stylesheet (Bulma, the Nomad 1.6 styles and the HashiCorp
//   Design System components, compiled with the new UI's styles to
//   assets/nomad-ui.css) is scoped to `.legacy` up to the marker comment at
//   the end of app.scss: every selector gets the `.legacy` ancestor, html/body/:root
//   rules apply to the `.legacy` element itself. Pages that are not migrated
//   yet render inside a `.legacy` container; the new frame and the new pages
//   never see Bulma.
// - The `legacy-reset` layer reverts the Tabler rules for the class names
//   that both frameworks use (table, tag, tooltip, progress, ...), inside
//   `.legacy` only.
//
// The legacy scope is removed together with Bulma once every page is
// migrated (UI plan, phase 4).

const path = require('path');
const fs = require('fs');
const Plugin = require('broccoli-plugin');
const Funnel = require('broccoli-funnel');
const { scopeLegacyCSS, wrapInLayer, LAYER_ORDER } = require('./scope');

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

class LegacyScope extends Plugin {
  constructor(input, { files }) {
    super([input], {
      annotation: 'norca-styles: legacy scope',
      persistentOutput: false,
    });
    this.files = files;
  }

  build() {
    const input = this.inputPaths[0];
    copyTree(input, this.outputPath);
    for (const file of this.files) {
      const target = path.join(this.outputPath, file);
      if (!fs.existsSync(target)) continue;
      const css = fs.readFileSync(target, 'utf8');
      fs.unlinkSync(target);
      fs.writeFileSync(target, scopeLegacyCSS(css));
    }
  }
}

function copyTree(from, to) {
  for (const entry of fs.readdirSync(from, { withFileTypes: true })) {
    const src = path.join(from, entry.name);
    const dest = path.join(to, entry.name);
    if (entry.isDirectory()) {
      fs.mkdirSync(dest, { recursive: true });
      copyTree(src, dest);
    } else {
      fs.copyFileSync(src, dest);
    }
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

  postprocessTree(type, tree) {
    if (type !== 'css') return tree;
    return new LegacyScope(tree, { files: ['assets/nomad-ui.css'] });
  },
};
