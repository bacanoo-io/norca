/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { htmlSafe } from '@ember/template';
import { ALL_REGIONS, ALL_NAMESPACES } from 'nomad-ui/services/frame';
import config from 'nomad-ui/config/environment';

// The top bar: command palette, region and namespace scope, notifications
// from the event stream, color theme, user and token.
export default class FrameTopbar extends Component {
  @service frame;
  @service system;
  @service token;
  @service store;
  @service router;

  get regionOptions() {
    const regions = (this.system.regions || []).slice().sort();
    return [
      { value: ALL_REGIONS, label: 'All regions' },
      ...regions.map((r) => ({ value: r, label: r })),
    ];
  }

  // r 1, r 2, ...: switch to the n-th region (keyboard service).
  get regionKeyCommands() {
    const regions = (this.system.regions || []).slice().sort();
    if (regions.length <= 1) return [];
    return regions.map((region, i) => ({
      label: `Switch to ${region} region`,
      pattern: ['r', `${i + 1}`],
      action: () => this.frame.selectRegion(region),
    }));
  }

  get regionValue() {
    return this.frame.regionScope;
  }

  get namespaceOptions() {
    const namespaces = (this.system.namespaces || [])
      .map((ns) => ns.get('name'))
      .filter(Boolean)
      .sort();
    return [
      { value: ALL_NAMESPACES, label: 'All namespaces' },
      ...namespaces.map((n) => ({ value: n, label: n })),
    ];
  }

  // Show the sign-in link or the token menu when ACLs are enabled, or when
  // the agent configuration cannot be read (ACLs on, no token yet).
  get aclEnabled() {
    return (
      !this.system.agent?.get('config') ||
      this.system.agent?.get('config.ACL.Enabled') === true
    );
  }

  get uiConfig() {
    return this.system.agent?.get('config.UI') || {};
  }

  get labelStyle() {
    const label = this.uiConfig.Label || {};
    return htmlSafe(
      `color: ${label.TextColor || 'inherit'}; background-color: ${
        label.BackgroundColor || 'transparent'
      };`
    );
  }

  get legacyURL() {
    return `${config.rootURL || '/ui/'}legacy/`;
  }

  @action
  signOut() {
    this.token.setProperties({ secret: undefined });
    // Clear out all data to ensure only data the anonymous token may see is shown.
    this.store.unloadAll();
    this.token.reset();
    this.router.transitionTo('jobs.index');
  }
}
