/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { navigationItems } from 'nomad-ui/utils/navigation';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';

const SEARCH_DELAY_MS = config.environment === 'test' ? 0 : 200;
const MAX_PER_CONTEXT = 5;

function matches(text, query) {
  return text.toLowerCase().includes(query.toLowerCase());
}

/**
 * The command palette (Ctrl+K): navigation, search and actions in one
 * keyboard-driven list. Search uses the fuzzy search API when the cluster
 * has it enabled.
 */
export default class FrameCommandPalette extends Component {
  @service frame;
  @service router;
  @service token;
  @service system;
  @service keyboard;
  @service abilities;

  @tracked query = '';
  @tracked activeIndex = 0;
  @tracked searchResults = [];
  @tracked isSearching = false;

  listId = `${guidFor(this)}-list`;
  searchTimer = null;
  pendingResolve = null;
  searchSerial = 0;

  get commands() {
    const pages = navigationItems()
      .filter((item) => !item.ability || this.abilities.can(item.ability))
      .map((item) => ({
        id: `page-${item.id}`,
        section: 'Pages',
        label: item.label,
        hint: item.group,
        icon: item.icon,
        run: () => this.router.transitionTo(item.route),
      }));

    const actions = [
      {
        id: 'run-job',
        section: 'Actions',
        label: 'Run a job',
        icon: 'player-play',
        run: () => this.router.transitionTo('jobs.run'),
      },
      {
        id: 'token',
        section: 'Actions',
        label: this.token.selfToken ? 'Profile and token' : 'Sign in',
        icon: 'key',
        run: () => this.router.transitionTo('settings.tokens'),
      },
      {
        id: 'theme',
        section: 'Actions',
        label:
          this.frame.effectiveTheme === 'dark'
            ? 'Switch to light mode'
            : 'Switch to dark mode',
        icon: this.frame.effectiveTheme === 'dark' ? 'sun' : 'moon',
        run: () => this.frame.toggleTheme(),
      },
      {
        id: 'shortcuts',
        section: 'Actions',
        label: 'Show keyboard shortcuts',
        icon: 'info-circle',
        run: () => this.showShortcuts(),
      },
      {
        id: 'legacy',
        section: 'Actions',
        label: 'Open the previous UI',
        icon: 'history',
        run: () => window.location.assign(`${config.rootURL || '/ui/'}legacy/`),
      },
    ];

    const regions = this.system.shouldShowRegions
      ? [
          {
            id: 'region-*',
            section: 'Regions',
            label: 'All regions',
            icon: 'world',
            run: () => this.frame.selectRegion('*'),
          },
          ...(this.system.regions || [])
            .slice()
            .sort()
            .map((region) => ({
              id: `region-${region}`,
              section: 'Regions',
              label: `Switch to ${region}`,
              icon: 'world',
              run: () => this.frame.selectRegion(region),
            })),
        ]
      : [];

    return [...pages, ...actions, ...regions];
  }

  get results() {
    const query = this.query.trim();
    const commands = query
      ? this.commands.filter(
          (c) => matches(c.label, query) || matches(c.hint || '', query)
        )
      : this.commands;
    return [...commands, ...this.searchResults].map((item, index) => ({
      ...item,
      index,
      domId: `${this.listId}-${index}`,
      isActive: index === this.activeIndex,
    }));
  }

  get sections() {
    const sections = [];
    for (const item of this.results) {
      let section = sections.find((s) => s.name === item.section);
      if (!section) {
        section = { name: item.section, items: [] };
        sections.push(section);
      }
      section.items.push(item);
    }
    return sections;
  }

  get activeDescendant() {
    return this.results[this.activeIndex]?.domId;
  }

  showShortcuts() {
    this.keyboard.shortcutsVisible = true;
  }

  @action
  input(event) {
    this.setQuery(event.target.value);
  }

  @action
  backdropClick(event) {
    if (event.target === event.currentTarget) this.close();
  }

  @action
  setQuery(value) {
    this.query = value;
    this.activeIndex = 0;
    this.scheduleSearch(value);
  }

  scheduleSearch(value) {
    this.cancelPendingSearch();
    const text = value.trim();
    if (text.length < 2 || !this.system.fuzzySearchEnabled) {
      this.searchResults = [];
      this.isSearching = false;
      return;
    }
    this.isSearching = true;
    // Registered with the test waiters, so settled() includes the search.
    waitForPromise(
      new Promise((resolve) => {
        this.pendingResolve = resolve;
        this.searchTimer = setTimeout(() => {
          this.pendingResolve = null;
          resolve(this.search(text));
        }, SEARCH_DELAY_MS);
      })
    );
  }

  cancelPendingSearch() {
    clearTimeout(this.searchTimer);
    this.pendingResolve?.();
    this.pendingResolve = null;
  }

  async search(text) {
    const serial = ++this.searchSerial;
    try {
      const response = await this.token.authorizedRequest('/v1/search/fuzzy', {
        method: 'POST',
        body: JSON.stringify({ Text: text, Context: 'all', Namespace: '*' }),
      });
      const json = await response.json();
      if (serial !== this.searchSerial || this.isDestroyed) return;
      this.searchResults = searchItems(json.Matches || {}, this.router);
    } catch (e) {
      if (serial === this.searchSerial) this.searchResults = [];
    } finally {
      if (serial === this.searchSerial && !this.isDestroyed)
        this.isSearching = false;
    }
  }

  @action
  keydown(event) {
    const count = this.results.length;
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (count) this.activeIndex = (this.activeIndex + 1) % count;
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      if (count) this.activeIndex = (this.activeIndex - 1 + count) % count;
    } else if (event.key === 'Enter') {
      event.preventDefault();
      const item = this.results[this.activeIndex];
      if (item) this.choose(item);
    }
  }

  @action
  choose(item) {
    this.close();
    item.run();
  }

  @action
  close() {
    this.cancelPendingSearch();
    this.searchSerial++;
    this.frame.closePalette();
  }

  willDestroy() {
    this.cancelPendingSearch();
    super.willDestroy(...arguments);
  }
}

function searchItems(matches, router) {
  const items = [];
  const take = (list) => (list || []).slice(0, MAX_PER_CONTEXT);

  for (const { ID: name, Scope: [namespace, id] = [] } of take(matches.jobs)) {
    items.push({
      id: `job-${namespace}-${id}`,
      section: 'Jobs',
      label: name,
      hint: namespace,
      icon: 'briefcase',
      run: () => router.transitionTo('jobs.job', `${id}@${namespace}`),
    });
  }
  for (const { ID: name, Scope: [id] = [] } of take(matches.nodes)) {
    items.push({
      id: `node-${id}`,
      section: 'Clients',
      label: name,
      icon: 'server-2',
      run: () => router.transitionTo('clients.client', id),
    });
  }
  for (const { ID: name, Scope: [namespace, id] = [] } of take(
    matches.allocs
  )) {
    items.push({
      id: `alloc-${id}`,
      section: 'Allocations',
      label: name,
      hint: namespace,
      icon: 'box',
      run: () => router.transitionTo('allocations.allocation', id),
    });
  }
  for (const { ID: group, Scope: [namespace, jobId] = [] } of take(
    matches.groups
  )) {
    items.push({
      id: `group-${namespace}-${jobId}-${group}`,
      section: 'Task groups',
      label: `${jobId} / ${group}`,
      hint: namespace,
      icon: 'stack-2',
      run: () =>
        router.transitionTo(
          'jobs.job.task-group',
          `${jobId}@${namespace}`,
          group
        ),
    });
  }
  for (const { ID: id } of take(matches.plugins)) {
    items.push({
      id: `plugin-${id}`,
      section: 'CSI plugins',
      label: id,
      icon: 'database',
      run: () => router.transitionTo('csi.plugins.plugin', id),
    });
  }
  return items;
}
