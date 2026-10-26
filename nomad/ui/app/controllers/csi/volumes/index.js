/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import { action, computed } from '@ember/object';
import { alias, readOnly } from '@ember/object/computed';
import { scheduleOnce } from '@ember/runloop';
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
    'id',
    'schedulable',
    'controllersHealthyProportion',
    'nodesHealthyProportion',
    'provider',
  ]),
  Searchable,
  WithListActions
) {
  @service system;
  @service router;
  @service userSettings;
  @service keyboard;
  @controller('csi/volumes') volumesController;

  @alias('volumesController.isForbidden')
  isForbidden;

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
    {
      qpNamespace: 'namespace',
    },
  ];

  viewParams = ['searchTerm', 'sortProperty', 'sortDescending'];

  get viewState() {
    return viewStateOf(this);
  }

  @computed('system.shouldShowNamespaces')
  get columns() {
    return [
      { key: 'name', label: 'Name', sortable: true, hideable: false },
      ...(this.system.shouldShowNamespaces
        ? [{ key: 'namespace', label: 'Namespace', sortable: true }]
        : []),
      { key: 'schedulable', label: 'Volume Health', sortable: true },
      {
        key: 'controllersHealthyProportion',
        label: 'Controller Health',
        sortable: true,
      },
      { key: 'nodesHealthyProportion', label: 'Node Health', sortable: true },
      { key: 'provider', label: 'Provider', sortable: true },
      { key: 'allocations', label: '# Allocs' },
    ];
  }

  currentPage = 1;
  @readOnly('userSettings.pageSize') pageSize;

  sortProperty = 'id';
  sortDescending = false;

  @computed
  get searchProps() {
    return ['name'];
  }

  @computed
  get fuzzySearchProps() {
    return ['name'];
  }

  fuzzySearchEnabled = true;

  @computed('qpNamespace', 'model.namespaces.[]')
  get optionsNamespaces() {
    const availableNamespaces = this.model.namespaces.map((namespace) => ({
      key: namespace.name,
      label: namespace.name,
    }));

    availableNamespaces.unshift({
      key: '*',
      label: 'All (*)',
    });

    // Unset the namespace selection if it was server-side deleted
    if (!availableNamespaces.mapBy('key').includes(this.qpNamespace)) {
      // eslint-disable-next-line ember/no-incorrect-calls-with-inline-anonymous-functions
      scheduleOnce('actions', () => {
        // eslint-disable-next-line ember/no-side-effects
        this.set('qpNamespace', '*');
      });
    }

    return availableNamespaces;
  }

  /**
    Visible volumes are those that match the selected namespace
  */
  @computed('model.volumes.@each.parent', 'system.{namespaces.length}')
  get visibleVolumes() {
    if (!this.model.volumes) return [];
    return this.model.volumes.compact();
  }

  @alias('visibleVolumes') listToSort;
  @alias('listSorted') listToSearch;
  @alias('listSearched') sortedVolumes;

  get namespaceSelectOptions() {
    return (this.optionsNamespaces || []).map((o) => ({
      value: o.key,
      label: o.label,
    }));
  }

  @action
  setNamespace(namespace) {
    this.setProperties({ qpNamespace: namespace, currentPage: 1 });
  }

  @action
  gotoVolume(volume) {
    this.router.transitionTo(
      'csi.volumes.volume',
      volume.get('idWithNamespace')
    );
  }
}
