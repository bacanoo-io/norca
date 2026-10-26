/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';

export default class UiBreadcrumb extends Component {
  @service router;

  // The "Go up a level" shortcut is bound to the item before the current
  // page (unless @shortcut is false, for a second trail on the page).
  isUpTarget = (index) => {
    if (this.args.shortcut === false) return false;
    const items = this.args.items || [];
    return index === items.length - 2 && !!items[index]?.route;
  };

  @action
  goTo(item) {
    const models =
      item.models || (item.model !== undefined ? [item.model] : []);
    this.router.transitionTo(item.route, ...models, {
      queryParams: item.query || {},
    });
  }
}
