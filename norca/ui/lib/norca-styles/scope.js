/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

const postcss = require('postcss');

// Cascade layers, lowest priority first. Unlayered styles (the Norca
// tokens and components) win over them.
const LAYER_ORDER = '@layer tabler;';

function wrapInLayer(css, layer) {
  const root = postcss.parse(css);
  root.walkAtRules('charset', (at) => at.remove());
  return `@layer ${layer} {\n${root.toString()}\n}\n`;
}

module.exports = {
  wrapInLayer,
  LAYER_ORDER,
};
