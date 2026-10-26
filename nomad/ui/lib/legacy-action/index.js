/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

// Ember 6 removed the {{action}} helper and modifier (RFC 1006). The pages
// inherited from the Nomad 1.6 UI use them a few hundred times; until those
// pages are migrated, this in-repo addon keeps them working:
//
// - a template AST transform rewrites every `action` invocation to the
//   app's `legacy-action` helper and `legacy-action-on` modifier, passing the
//   template's `this` as the first argument (the context the built-in keyword
//   used to receive implicitly);
// - `(action (mut x) ...)` becomes `(legacy-action this (fn (mut x)) ...)`,
//   because only `fn` can turn a `mut` reference into a setter.
//
// See app/helpers/legacy-action.js and app/modifiers/legacy-action-on.js.
// New code uses `on` and `fn`. Remove this addon with the last legacy page
// (UI plan, phase 4).

function legacyActionTransform(env) {
  const b = env.syntax.builders;

  function rewriteParams(params) {
    const [first, ...rest] = params;
    let target = first;
    if (
      first &&
      first.type === 'SubExpression' &&
      first.path.original === 'mut'
    ) {
      target = b.sexpr(b.path('fn'), [first]);
    }
    return [b.path('this'), target, ...rest].filter(Boolean);
  }

  function isAction(node) {
    return (
      node.path &&
      node.path.type === 'PathExpression' &&
      node.path.original === 'action'
    );
  }

  return {
    name: 'legacy-action',
    visitor: {
      SubExpression(node) {
        if (!isAction(node)) return;
        return b.sexpr(
          b.path('legacy-action'),
          rewriteParams(node.params),
          node.hash,
          node.loc
        );
      },
      MustacheStatement(node) {
        if (!isAction(node)) return;
        return b.mustache(
          b.path('legacy-action'),
          rewriteParams(node.params),
          node.hash,
          node.trusting,
          node.loc,
          node.strip
        );
      },
      ElementModifierStatement(node) {
        if (!isAction(node)) return;
        return b.elementModifier(
          b.path('legacy-action-on'),
          rewriteParams(node.params),
          node.hash,
          node.loc
        );
      },
    },
  };
}

legacyActionTransform.cacheKey = () => 'legacy-action-v1';

module.exports = {
  name: 'legacy-action',

  isDevelopingAddon() {
    return true;
  },

  setupPreprocessorRegistry(type, registry) {
    if (type !== 'parent') return;
    registry.add('htmlbars-ast-plugin', {
      name: 'legacy-action',
      plugin: legacyActionTransform,
      baseDir() {
        return __dirname;
      },
      cacheKey() {
        return 'legacy-action-v1';
      },
      parallelBabel: {
        requireFile: __filename,
        buildUsing: 'buildPlugin',
        params: {},
      },
    });
  },

  buildPlugin() {
    return {
      name: 'legacy-action',
      plugin: legacyActionTransform,
      baseDir() {
        return __dirname;
      },
      cacheKey() {
        return 'legacy-action-v1';
      },
    };
  },

  legacyActionTransform,
};
