/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { modifier } from 'ember-modifier';
import { join } from '@ember/runloop';
import { resolveAction } from 'nomad-ui/helpers/legacy-action';

const MODIFIER_KEYS = ['alt', 'ctrl', 'meta', 'shift'];

function isAllowed(event, allowedKeys) {
  if (allowedKeys === 'any') return true;
  return MODIFIER_KEYS.every(
    (key) => !event[`${key}Key`] || (allowedKeys || '').includes(key)
  );
}

/**
 * The replacement of the `{{action}}` element modifier removed in Ember 6.
 * Templates do not use it directly: lib/legacy-action rewrites
 * `<div {{action fn ...args}}>` to `<div {{legacy-action-on this fn ...args}}>`.
 *
 * Same behavior as the original: `on=` (default click), `preventDefault=`
 * (default true), `bubbles=` (default true), `allowedKeys=` (by default the
 * action does not run while a modifier key is held), curried arguments, the
 * template's context (or `target=`) as `this`.
 */
export default modifier(
  (
    element,
    [context, action, ...curried],
    { on = 'click', preventDefault = true, bubbles = true, allowedKeys, target }
  ) => {
    const owner = target || context;
    const fn = resolveAction(context, action, target);
    const handler = (event) => {
      if (!isAllowed(event, allowedKeys)) return;
      if (preventDefault !== false) event.preventDefault();
      if (bubbles === false) event.stopPropagation();
      join(owner, fn, ...curried);
    };
    element.addEventListener(on, handler);
    return () => element.removeEventListener(on, handler);
  }
);
