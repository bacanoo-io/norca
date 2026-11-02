/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Mixin from '@ember/object/mixin';
import { action } from '@ember/object';
import { serialize } from 'nomad-ui/utils/qp-serialize';

/**
 * The actions of a list controller on the unified list (Ui::DataTable):
 * sorting, paging, searching and filtering through the controller's query
 * parameters (Sortable, Searchable and the facet query parameters), and the
 * saved views.
 *
 * The controller lists the properties a saved view keeps in `viewParams`
 * and exposes `viewState` (see `viewStateOf`).
 */
// The list controllers are classic and built from mixins (Sortable,
// Searchable); the list actions follow the same pattern.
// eslint-disable-next-line ember/no-new-mixins
export default Mixin.create({
  viewParams: null,

  sort: action(function (property, descending) {
    this.setProperties({
      sortProperty: property,
      sortDescending: descending,
    });
  }),

  setPage: action(function (page) {
    this.set('currentPage', page);
  }),

  setPageSize: action(function (size) {
    if (this.userSettings) {
      this.userSettings.set('pageSize', size);
    } else {
      this.set('pageSize', size);
    }
    this.set('currentPage', 1);
  }),

  search: action(function (term) {
    this.set('searchTerm', term);
    this.set('currentPage', 1);
  }),

  setFilter: action(function (queryParam, selection) {
    this.set(queryParam, serialize(selection));
    this.set('currentPage', 1);
  }),

  applyView: action(function (state) {
    const params = this.viewParams || [];
    const values = {};
    params.forEach((key) => {
      if (state && key in state) values[key] = state[key];
    });
    this.setProperties({ ...values, currentPage: 1 });
  }),
});

// The saved state of a list: the values of its view parameters.
export function viewStateOf(controller) {
  const state = {};
  (controller.viewParams || []).forEach((key) => {
    state[key] = controller.get(key);
  });
  return state;
}
