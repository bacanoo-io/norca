/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service, { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

const STORAGE = {
  sidebar: 'norcaSidebarCollapsed',
  theme: 'norcaTheme',
  scope: 'norcaRegionScope',
  namespace: 'norcaNamespace',
};

export const ALL_REGIONS = '*';
export const ALL_NAMESPACES = '*';

function read(key, fallback) {
  try {
    const value = window.localStorage.getItem(key);
    return value === null ? fallback : value;
  } catch (e) {
    return fallback;
  }
}

function write(key, value) {
  try {
    if (value === null || value === undefined)
      window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, String(value));
  } catch (e) {
    // storage unavailable (private mode): the setting lasts for the session
  }
}

/**
 * State of the global frame: sidebar, color theme, the region scope ("All
 * regions" or the active region) and the namespace scope, and the command
 * palette. The active region itself stays in the system service, which the
 * data layer uses for every request.
 */
export default class FrameService extends Service {
  @service system;
  @service router;

  @tracked sidebarCollapsed = read(STORAGE.sidebar, 'false') === 'true';
  @tracked mobileMenuOpen = false;
  @tracked theme = read(STORAGE.theme, 'auto');
  @tracked allRegions = read(STORAGE.scope, '') === ALL_REGIONS;
  @tracked namespace = read(STORAGE.namespace, ALL_NAMESPACES);
  @tracked paletteOpen = false;

  constructor() {
    super(...arguments);
    this.applyTheme();
    this.media = window.matchMedia?.('(prefers-color-scheme: dark)');
    this.onSchemeChange = () => this.applyTheme();
    this.media?.addEventListener?.('change', this.onSchemeChange);
    this.onKeydown = (event) => this.globalKeydown(event);
    document.addEventListener('keydown', this.onKeydown);
  }

  willDestroy() {
    this.media?.removeEventListener?.('change', this.onSchemeChange);
    document.removeEventListener('keydown', this.onKeydown);
    super.willDestroy(...arguments);
  }

  get effectiveTheme() {
    if (this.theme === 'light' || this.theme === 'dark') return this.theme;
    return this.media?.matches ? 'dark' : 'light';
  }

  applyTheme() {
    document.documentElement.setAttribute('data-bs-theme', this.effectiveTheme);
  }

  @action
  toggleTheme() {
    this.theme = this.effectiveTheme === 'dark' ? 'light' : 'dark';
    write(STORAGE.theme, this.theme);
    this.applyTheme();
  }

  @action
  toggleSidebar() {
    this.sidebarCollapsed = !this.sidebarCollapsed;
    write(STORAGE.sidebar, this.sidebarCollapsed);
  }

  @action
  toggleMobileMenu() {
    this.mobileMenuOpen = !this.mobileMenuOpen;
  }

  @action
  closeMobileMenu() {
    this.mobileMenuOpen = false;
  }

  // The region scope of the views that support it (the dashboard):
  // ALL_REGIONS or the active region.
  get regionScope() {
    return this.allRegions && this.system.shouldShowRegions
      ? ALL_REGIONS
      : this.system.activeRegion ||
          this.system.get('defaultRegion.region') ||
          '';
  }

  // The regions the region-aware views show.
  // An empty string stands for the agent's own region (no region parameter).
  get scopedRegions() {
    const regions = (this.system.regions || []).slice().sort();
    if (!regions.length) return [this.system.get('defaultRegion.region') || ''];
    if (this.regionScope === ALL_REGIONS) return regions;
    const region = this.regionScope;
    return region ? [region] : regions.slice(0, 1);
  }

  get currentRouteSpansRegions() {
    return (
      this.router.currentRouteName === 'index' ||
      !!this.router.currentRoute?.metadata?.spansRegions
    );
  }

  @action
  selectRegion(value) {
    if (value === ALL_REGIONS) {
      this.allRegions = true;
      write(STORAGE.scope, ALL_REGIONS);
      // The dashboard and the pages whose route metadata says spansRegions
      // show all regions; other pages go to the dashboard.
      if (!this.currentRouteSpansRegions) this.router.transitionTo('index');
      return;
    }
    this.allRegions = false;
    write(STORAGE.scope, null);
    const target = this.currentRouteSpansRegions
      ? this.router.currentRouteName
      : 'jobs';
    this.router.transitionTo(target, { queryParams: { region: value } });
  }

  @action
  selectNamespace(value) {
    this.namespace = value || ALL_NAMESPACES;
    write(STORAGE.namespace, this.namespace);
  }

  // Ctrl+K (Cmd+K) anywhere, or / outside text fields, opens the palette.
  globalKeydown(event) {
    const key = event.key?.toLowerCase();
    if (key === 'k' && (event.ctrlKey || event.metaKey) && !event.altKey) {
      event.preventDefault();
      this.paletteOpen ? this.closePalette() : this.openPalette();
      return;
    }
    if (event.key === '/' && !this.paletteOpen) {
      const target = event.target;
      const tag = target?.nodeName?.toLowerCase();
      if (
        tag === 'input' ||
        tag === 'textarea' ||
        tag === 'select' ||
        target?.isContentEditable
      )
        return;
      event.preventDefault();
      this.openPalette();
    }
  }

  @action
  openPalette() {
    this.paletteOpen = true;
  }

  @action
  closePalette() {
    this.paletteOpen = false;
  }
}
