/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { get } from '@ember/object';

function compare(a, b) {
  if (a === b) return 0;
  if (a === undefined || a === null || a === '') return 1;
  if (b === undefined || b === null || b === '') return -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (a instanceof Date && b instanceof Date) return a - b;
  return String(a).localeCompare(String(b), undefined, { numeric: true });
}

/**
 * Sorts the rows of a list by a property path (Ember `get` semantics, so
 * records, proxies and plain objects work), keeping empty values last.
 */
export function sortRows(rows, path, descending = false) {
  const list =
    rows && typeof rows.toArray === 'function'
      ? rows.toArray()
      : [...(rows || [])];
  const sorted = list
    .map((row, index) => ({ row, index, value: get(row, path) }))
    .sort((x, y) => compare(x.value, y.value) || x.index - y.index);
  if (descending) {
    // Empty values stay last when descending, too.
    const filled = sorted.filter(
      (x) => x.value !== undefined && x.value !== null && x.value !== ''
    );
    const empty = sorted.filter(
      (x) => x.value === undefined || x.value === null || x.value === ''
    );
    return [...filled.reverse(), ...empty].map((x) => x.row);
  }
  return sorted.map((x) => x.row);
}
