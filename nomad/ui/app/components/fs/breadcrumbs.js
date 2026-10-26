/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@ember/component';
import { computed } from '@ember/object';
import { isEmpty } from '@ember/utils';
import { tagName } from '@ember-decorators/component';
import classic from 'ember-classic-decorator';

@classic
@tagName('')
export default class Breadcrumbs extends Component {
  allocation = null;
  taskState = null;
  path = null;

  @computed('path')
  get breadcrumbs() {
    const breadcrumbs = this.path
      .split('/')
      .reject(isEmpty)
      .reduce((breadcrumbs, pathSegment, index) => {
        let breadcrumbPath;

        if (index > 0) {
          const lastBreadcrumb = breadcrumbs[index - 1];
          breadcrumbPath = `${lastBreadcrumb.path}/${pathSegment}`;
        } else {
          breadcrumbPath = pathSegment;
        }

        breadcrumbs.push({
          name: pathSegment,
          path: breadcrumbPath,
        });

        return breadcrumbs;
      }, []);

    if (breadcrumbs.length) {
      breadcrumbs[breadcrumbs.length - 1].isLast = true;
    }

    return breadcrumbs;
  }

  // The trail for Ui::Breadcrumb: the allocation or task, then each
  // directory of the path.
  @computed('allocation', 'breadcrumbs', 'taskState')
  get items() {
    const { allocation, taskState } = this;
    const route = (path) => {
      if (taskState) {
        return path
          ? {
              route: 'allocations.allocation.task.fs',
              models: [allocation, taskState, path],
            }
          : {
              route: 'allocations.allocation.task.fs-root',
              models: [allocation, taskState],
            };
      }
      return path
        ? { route: 'allocations.allocation.fs', models: [allocation, path] }
        : { route: 'allocations.allocation.fs-root', models: [allocation] };
    };
    return [
      {
        label: taskState ? taskState.name : allocation.shortId,
        id: 'fs-root',
        ...route(null),
      },
      ...this.breadcrumbs.map((breadcrumb, index) => ({
        label: breadcrumb.name,
        id: `fs-${index}`,
        ...route(breadcrumb.path),
      })),
    ];
  }
}
