/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from 'nomad-ui/abilities/abstract';
import { capitalize } from '@ember/string';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';

/**
 * Builds an ember-can ability out of a table of checks instead of a class
 * written by hand. Every entry maps a verb ("list", "read", "restoreFull")
 * to a predicate of the signed-in token; the ability answers
 * `can <verb> <subject>` with that predicate, i.e. it gets a can<Verb>
 * getter per entry. The token is the one the abstract ability tracks.
 */
export function tokenAbility(checks) {
  const ability = class extends AbstractAbility {};
  for (const [verb, check] of Object.entries(checks)) {
    Object.defineProperty(ability.prototype, `can${capitalize(verb)}`, {
      configurable: true,
      get() {
        return check(this.token);
      },
    });
  }
  return ability;
}

/**
 * An ability for one of the Norca ACL blocks (Admission, Quota, Backup,
 * ...): `read` and `write` follow the block's level in the token's
 * policies; `extra` adds further verbs.
 */
export function blockAbility(block, extra = {}) {
  return tokenAbility({
    read: (token) => allowsBlock(token, block, 'read'),
    write: (token) => allowsBlock(token, block, 'write'),
    ...extra,
  });
}
