/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { NativeArray } from '@ember/array';

// Ember 6 no longer extends Array.prototype with the EmberArray methods
// (sortBy, mapBy, findBy, pushObject, ...). The pages inherited from the
// Nomad 1.6 UI use them in several hundred places, so until those pages are
// migrated to the new component layer we apply the mixin ourselves, exactly
// like Ember 5 did with EXTEND_PROTOTYPES.Array. New code must not rely on
// these methods; use native array methods instead.
//
// Remove this module once no legacy page depends on it (UI plan, phase 4).
export function applyArrayPrototypeExtensions() {
  if (!Array.prototype.mapBy) {
    // The second argument hides the mixed-in keys from enumeration.
    NativeArray.apply(Array.prototype, true);
  }
}

applyArrayPrototypeExtensions();
