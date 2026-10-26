/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

/**
 *   <Access::Header @title="Tokens" @trail={{array (hash label="New")}}>actions</Access::Header>
 *
 * @trail: the items after "Access control" (default: the title), for
 * example the list and the token name on a token page.
 */
export default class AccessHeader extends Component {
  get breadcrumbs() {
    const items = [
      {
        label: 'Access control',
        route: 'access-control.index',
        id: 'access-control.index',
      },
    ];
    return [...items, ...(this.args.trail || [{ label: this.args.title }])];
  }
}
