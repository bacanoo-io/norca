/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import { action, computed } from '@ember/object';
import { alias, readOnly } from '@ember/object/computed';
import Controller, { inject as controller } from '@ember/controller';
import SortableFactory from 'nomad-ui/mixins/sortable-factory';
import Searchable from 'nomad-ui/mixins/searchable';
import classic from 'ember-classic-decorator';
import WithListActions, {
  viewStateOf,
} from 'nomad-ui/mixins/with-list-actions';

@classic
export default class IndexController extends Controller.extend(
  SortableFactory([
    'plainId',
    'controllersHealthyProportion',
    'nodesHealthyProportion',
    'provider',
  ]),
  Searchable,
  WithListActions
) {
  @service userSettings;
  @service router;

  viewParams = ['searchTerm', 'sortProperty', 'sortDescending'];

  get viewState() {
    return viewStateOf(this);
  }

  columns = [
    { key: 'plainId', label: 'ID', sortable: true, hideable: false },
    {
      key: 'controllersHealthyProportion',
      label: 'Controller Health',
      sortable: true,
    },
    { key: 'nodesHealthyProportion', label: 'Node Health', sortable: true },
    { key: 'provider', label: 'Provider', sortable: true },
  ];
  @controller('csi/plugins') pluginsController;

  @alias('pluginsController.isForbidden') isForbidden;

  queryParams = [
    {
      currentPage: 'page',
    },
    {
      searchTerm: 'search',
    },
    {
      sortProperty: 'sort',
    },
    {
      sortDescending: 'desc',
    },
  ];

  currentPage = 1;
  @readOnly('userSettings.pageSize') pageSize;

  @computed
  get searchProps() {
    return ['id'];
  }

  @computed
  get fuzzySearchProps() {
    return ['id'];
  }

  sortProperty = 'id';
  sortDescending = false;

  @alias('model') listToSort;
  @alias('listSorted') listToSearch;
  @alias('listSearched') sortedPlugins;

  @action
  gotoPlugin(plugin) {
    this.router.transitionTo('csi.plugins.plugin', plugin.plainId);
  }
}
