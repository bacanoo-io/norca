/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { NAVIGATION } from 'nomad-ui/utils/navigation';

// The left menu: the groups of the information architecture, group headers
// are not clickable. Collapsible to icons on large screens, a toggled panel
// on small ones.
export default class FrameSidebar extends Component {
  @service frame;
  @service system;
  @service abilities;
  @service keyboard;

  get groups() {
    return NAVIGATION.map(({ group, items }) => ({
      group,
      items: items.filter(
        (item) => !item.ability || this.abilities.can(item.ability)
      ),
    })).filter(({ items }) => items.length);
  }
}
