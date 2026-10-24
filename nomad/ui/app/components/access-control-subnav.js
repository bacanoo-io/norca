/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

/**
 * Tabs for the Access Control section: overview, tokens, roles and
 * policies. Each tab is only shown when the current token may list that
 * kind of object.
 */
export default class AccessControlSubnavComponent extends Component {
  @service keyboard;
}
