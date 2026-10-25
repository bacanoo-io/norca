/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/**
 * Table columns of the Access Control pages. A column may carry `requires`,
 * an ability ("list tokens", "destroy role", ...): it is only shown when the
 * signed-in token has that ability. `requires` is stripped from the result.
 */
export function visibleColumns(can, columns) {
  return columns
    .filter((column) => !column.requires || can.can(column.requires))
    .map(({ requires, ...column }) => column); // eslint-disable-line no-unused-vars
}

/**
 * The trailing column of the row actions (the delete button), shown to
 * whoever may destroy records of the given type.
 */
export function actionsColumn(type) {
  return {
    key: 'actions',
    label: 'Actions',
    srOnly: true,
    hideable: false,
    requires: `destroy ${type}`,
  };
}
