/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

/**
 * Tabs, either route-based or local.
 *
 * Route tabs (navigation, rendered as links):
 *   <Ui::Tabs @tabs={{array (hash label="Overview" route="jobs.job.index")
 *                           (hash label="Versions" route="jobs.job.versions" count=3)}} />
 *
 * Local tabs (a tablist; the caller renders the panel of @selected):
 *   <Ui::Tabs @tabs={{this.tabs}} @selected={{this.tab}} @onSelect={{this.selectTab}} />
 *
 * Each tab: { id, label, route?, model?, query?, count?, icon? }. Local tabs
 * follow the WAI-ARIA tabs pattern: arrow keys, Home and End move between
 * the tabs.
 */
export default class UiTabs extends Component {
  get isRouteBased() {
    return (this.args.tabs || []).some((tab) => tab.route);
  }

  get tabs() {
    return (this.args.tabs || []).map((tab, index) => {
      const id = tab.id ?? tab.route ?? String(index);
      return {
        ...tab,
        id,
        models: tab.model !== undefined ? [tab.model] : tab.models || [],
        query: tab.query || {},
        isSelected: id === this.args.selected,
      };
    });
  }

  @action
  select(tab) {
    this.args.onSelect?.(tab.id);
  }

  @action
  keydown(event) {
    const keys = ['ArrowLeft', 'ArrowRight', 'Home', 'End'];
    if (!keys.includes(event.key)) return;
    const buttons = [
      ...event.currentTarget.querySelectorAll('[role="tab"]:not([disabled])'),
    ];
    const current = buttons.indexOf(document.activeElement);
    let next = current;
    if (event.key === 'ArrowRight') next = (current + 1) % buttons.length;
    if (event.key === 'ArrowLeft')
      next = (current - 1 + buttons.length) % buttons.length;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = buttons.length - 1;
    event.preventDefault();
    buttons[next]?.focus();
    buttons[next]?.click();
  }
}
