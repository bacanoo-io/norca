/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import breadcrumbItems from 'nomad-ui/utils/breadcrumb-items';

export default class VariablesVariableController extends Controller {
  get breadcrumbItems() {
    return [
      { label: 'Variables', route: 'variables.index', id: 'variables.index' },
      ...breadcrumbItems(this.breadcrumbs),
    ];
  }

  get breadcrumbs() {
    let crumbs = [];
    let id = decodeURI(this.params.id.split('@').slice(0, -1).join('@')); // remove namespace
    let namespace = this.params.id.split('@').slice(-1)[0];
    id.split('/').reduce((m, n) => {
      crumbs.push({
        label: n,
        args:
          m + n === id // If the last crumb, link to the var itself
            ? [`variables.variable`, `${m + n}@${namespace}`]
            : [`variables.path`, m + n],
      });
      return m + n + '/';
    }, []);
    return crumbs;
  }
}
