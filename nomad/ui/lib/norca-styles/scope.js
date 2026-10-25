/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

const postcss = require('postcss');

const SCOPE = '.legacy';

// Cascade layers, lowest priority first. Unlayered styles (the legacy
// stylesheet, the Norca tokens and components) win over both.
const LAYER_ORDER = '@layer tabler, legacy-reset;';

// Class names that Tabler (Bootstrap) and the legacy stylesheet both use.
// Inside `.legacy`, Tabler's declarations for them are reverted so that only
// the legacy rules apply.
const SHARED_CLASSES = [
  'accordion-body',
  'badge',
  'border',
  'breadcrumb',
  'chart',
  'collapse',
  'dropdown',
  'dropdown-menu',
  'dropdown-item',
  'icon',
  'legend',
  'modal',
  'navbar',
  'navbar-brand',
  'navbar-end',
  'navbar-item',
  'page-body',
  'page-header',
  'pagination',
  'progress',
  'status',
  'table',
  'tag',
  'timeline',
  'toolbar',
  'tooltip',
];

// Declarations of html/body that must not apply to the `.legacy` container.
const ROOT_ONLY_PROPS = new Set([
  'overflow',
  'overflow-x',
  'overflow-y',
  'min-width',
  'min-height',
  'height',
  '-webkit-text-size-adjust',
  '-moz-text-size-adjust',
  'text-size-adjust',
  'scroll-behavior',
]);

const ROOT_SELECTOR = /^(html|body|:root)(?![\w-])/;

function scopeSelector(selector) {
  const trimmed = selector.trim();
  if (!trimmed) return trimmed;
  if (!ROOT_SELECTOR.test(trimmed)) return `${SCOPE} ${trimmed}`;
  // `html`, `html body`, `body.is-x .y`, `body > .y`: the root part
  // becomes the `.legacy` element itself.
  let rest = trimmed;
  let match;
  while ((match = rest.match(ROOT_SELECTOR))) {
    rest = rest.slice(match[0].length);
    const ws = rest.match(/^\s+(?=(html|body|:root)(?![\w-]))/);
    if (ws) rest = rest.slice(ws[0].length);
  }
  if (!rest) return SCOPE;
  if (/^[\s>+~]/.test(rest)) return `${SCOPE} ${rest.trim()}`;
  return `${SCOPE}${rest}`;
}

function isInsideKeyframes(rule) {
  let parent = rule.parent;
  while (parent) {
    if (parent.type === 'atrule' && /keyframes$/i.test(parent.name))
      return true;
    parent = parent.parent;
  }
  return false;
}

// app.scss ends with this marker comment followed by the styles of the new
// UI, which stay global.
const UNSCOPED_MARKER = 'norca-styles: unscoped below';

function scopeLegacyCSS(css) {
  const root = postcss.parse(css);
  root.walkAtRules('charset', (at) => at.remove());

  const markerIndex = root.nodes.findIndex(
    (node) => node.type === 'comment' && node.text.includes(UNSCOPED_MARKER)
  );
  const unscoped = new Set(
    markerIndex >= 0 ? root.nodes.slice(markerIndex) : []
  );
  const topLevel = (node) => {
    while (node.parent && node.parent !== root) node = node.parent;
    return node;
  };

  root.walkRules((rule) => {
    if (isInsideKeyframes(rule)) return;
    if (unscoped.has(topLevel(rule))) return;
    const selectors = rule.selectors;
    const rootLevel = selectors.every(
      (s) => ROOT_SELECTOR.test(s.trim()) && scopeSelector(s) === SCOPE
    );
    rule.selectors = selectors.map(scopeSelector);
    if (rootLevel) {
      rule.walkDecls((decl) => {
        if (ROOT_ONLY_PROPS.has(decl.prop.toLowerCase())) decl.remove();
      });
      if (!rule.nodes.length) rule.remove();
    }
  });
  const reset = SHARED_CLASSES.map((c) => `${SCOPE} .${c}`).join(',\n');
  const out = root.toString();
  return `${LAYER_ORDER}
@layer legacy-reset {
${reset},
${SCOPE} .table > :not(caption) > * > * {
  all: revert;
}
}
${out}`;
}

function wrapInLayer(css, layer) {
  const root = postcss.parse(css);
  root.walkAtRules('charset', (at) => at.remove());
  return `@layer ${layer} {\n${root.toString()}\n}\n`;
}

module.exports = {
  scopeLegacyCSS,
  scopeSelector,
  wrapInLayer,
  LAYER_ORDER,
  SCOPE,
};
