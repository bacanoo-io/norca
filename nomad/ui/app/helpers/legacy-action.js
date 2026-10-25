/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { helper } from '@ember/component/helper';
import { get } from '@ember/object';
import { join } from '@ember/runloop';
import { assert } from '@ember/debug';

// Resolves the function an {{action}} invocation refers to: a function, or
// the name of an action of the target (`actions` hash or method).
export function resolveAction(context, action, target) {
  const owner = target || context;
  if (typeof action === 'string') {
    const fn = owner?.actions?.[action] || owner?.[action];
    assert(
      `legacy-action: no action named "${action}" on ${owner}`,
      typeof fn === 'function'
    );
    return fn;
  }
  assert(
    `legacy-action: expected a function or an action name, got ${action}`,
    typeof action === 'function'
  );
  return action;
}

/**
 * The replacement of the `(action)` helper removed in Ember 6. Templates do
 * not call it directly: lib/legacy-action rewrites `(action fn ...args)` to
 * `(legacy-action this fn ...args)`.
 *
 * Like the original it binds the function to the template's context (or
 * `target=`), curries the extra arguments, picks a property of the first
 * call argument with `value=` and runs the call in a run loop.
 */
export function legacyAction(
  [context, action, ...curried],
  { value, target } = {}
) {
  const owner = target || context;
  const fn = resolveAction(context, action, target);
  return function (...args) {
    let callArgs = [...curried, ...args];
    if (value) {
      callArgs = [...curried, get(args[0], value)];
    }
    return join(owner, fn, ...callArgs);
  };
}

export default helper(legacyAction);
