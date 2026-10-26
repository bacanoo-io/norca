/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { alias } from '@ember/object/computed';
import Controller from '@ember/controller';
import WithNamespaceResetting from 'nomad-ui/mixins/with-namespace-resetting';
import Sortable from 'nomad-ui/mixins/sortable';
import classic from 'ember-classic-decorator';
import WithListActions from 'nomad-ui/mixins/with-list-actions';

@classic
export default class EvaluationsController extends Controller.extend(
  WithNamespaceResetting,
  Sortable,
  WithListActions
) {
  columns = [
    { key: 'id', label: 'ID', hideable: false },
    { key: 'priority', label: 'Priority', sortable: true },
    { key: 'createTime', label: 'Created', sortable: true },
    { key: 'triggeredBy', label: 'Triggered by', sortable: true },
    { key: 'status', label: 'Status', sortable: true },
    {
      key: 'hasPlacementFailures',
      label: 'Placement failures',
      sortable: true,
    },
  ];

  queryParams = [
    {
      sortProperty: 'sort',
    },
    {
      sortDescending: 'desc',
    },
  ];

  sortProperty = 'modifyIndex';
  sortDescending = true;

  @alias('model') job;
  @alias('model.evaluations') evaluations;

  @alias('evaluations') listToSort;
  @alias('listSorted') sortedEvaluations;
}
