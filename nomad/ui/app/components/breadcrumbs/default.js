/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { action } from '@ember/object';
import Component from '@glimmer/component';
import KeyboardShortcutModifier from 'nomad-ui/modifiers/keyboard-shortcut';
import { inject as service } from '@ember/service';

export default class BreadcrumbsTemplate extends Component {
  @service router;

  @action
  traverseUpALevel(args) {
    const [path, ...rest] = args;
    const last = rest[rest.length - 1];
    if (last && last.isQueryParams) {
      rest[rest.length - 1] = { queryParams: last.values };
    }
    this.router.transitionTo(path, ...rest);
  }

  // crumb.args is the old positional LinkTo form: [route, ...models, qp?]
  // where qp is a query-params object from utils/classes/query-params.
  get linkRoute() {
    return this.args.crumb.args[0];
  }

  get linkModels() {
    const rest = this.args.crumb.args.slice(1);
    const last = rest[rest.length - 1];
    return last && last.isQueryParams ? rest.slice(0, -1) : rest;
  }

  get linkQuery() {
    const args = this.args.crumb.args;
    const last = args[args.length - 1];
    return last && last.isQueryParams ? last.values : {};
  }

  get maybeKeyboardShortcut() {
    return this.args.isOneCrumbUp() ? KeyboardShortcutModifier : null;
  }
}
