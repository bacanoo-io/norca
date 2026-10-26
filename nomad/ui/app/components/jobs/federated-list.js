/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import config from 'nomad-ui/config/environment';
import { sortRows } from 'nomad-ui/utils/sort-rows';
import { ownerFromMeta } from 'nomad-ui/utils/job-owner';

const REFRESH_DEBOUNCE_MS = 1000;
const FALLBACK_POLL_MS = 30000;

const STATE_KEYS = [
  ['queued', 'Queued', 'Queued'],
  ['starting', 'Starting', 'Starting'],
  ['running', 'Running', 'Running'],
  ['complete', 'Complete', 'Complete'],
  ['unknown', 'Unknown', 'Unknown'],
  ['failed', 'Failed', 'Failed'],
  ['lost', 'Lost', 'Lost'],
];

function displayType(stub) {
  if (stub.Periodic) return 'periodic';
  if (stub.ParameterizedJob) return 'parameterized';
  return stub.Type;
}

function summaryOf(stub) {
  const groups = Object.values(stub.JobSummary?.Summary || {});
  return STATE_KEYS.map(([key, label, field]) => ({
    key,
    label,
    value: groups.reduce((sum, g) => sum + (g[field] || 0), 0),
  }));
}

/**
 * The jobs list in "All regions" mode (UI plan, jobs: a region column in
 * "All regions" mode): the job stubs of every region in scope, read with
 * one list request per region (the store keeps one region at a time).
 * Each row links to the job in its region. Live: the Job topic of the event
 * stream of each region refreshes that region; without a stream the list
 * polls every 30 seconds.
 */
export default class JobsFederatedList extends Component {
  @service token;
  @service eventStream;
  @service router;
  @service system;

  @tracked byRegion = {};
  @tracked errors = {};
  @tracked isLoading = true;
  @tracked searchTerm = '';
  @tracked selectionRegion = [];
  @tracked selectionStatus = [];
  @tracked selectionType = [];
  @tracked sortBy = 'name';
  @tracked sortDescending = false;
  @tracked page = 1;

  timers = new Map();
  @tracked pageSize = 25;

  columns = [
    { key: 'region', label: 'Region', sortable: true },
    { key: 'name', label: 'Name', sortable: true, hideable: false },
    { key: 'namespace', label: 'Namespace', sortable: true },
    { key: 'status', label: 'Status', sortable: true },
    { key: 'type', label: 'Type', sortable: true },
    { key: 'nodePool', label: 'Node pool', sortable: true, hidden: true },
    { key: 'priority', label: 'Priority', sortable: true },
    { key: 'owner', label: 'Managed by' },
    { key: 'summary', label: 'Allocations' },
  ];

  get namespace() {
    return this.args.namespace || '*';
  }

  get regions() {
    return (this.args.regions || []).filter(Boolean);
  }

  get rows() {
    return this.regions.flatMap((region) =>
      (this.byRegion[region] || [])
        .filter((stub) => !stub.ParentID)
        .map((stub) => ({
          key: `${region}:${stub.Namespace}:${stub.ID}`,
          region,
          id: stub.ID,
          name: stub.Name,
          namespace: stub.Namespace || 'default',
          status: stub.Status,
          type: displayType(stub),
          nodePool: stub.NodePool,
          priority: stub.Priority,
          owner: ownerFromMeta(stub.Meta),
          multiregion: !!stub.Multiregion?.Regions?.length,
          summary: summaryOf(stub),
          routeId: `${stub.ID}@${stub.Namespace || 'default'}`,
        }))
    );
  }

  get filteredRows() {
    const term = this.searchTerm.trim().toLowerCase();
    return this.rows.filter((row) => {
      if (term && !row.name.toLowerCase().includes(term)) return false;
      if (
        this.selectionRegion.length &&
        !this.selectionRegion.includes(row.region)
      ) {
        return false;
      }
      if (
        this.selectionStatus.length &&
        !this.selectionStatus.includes(row.status)
      ) {
        return false;
      }
      if (this.selectionType.length && !this.selectionType.includes(row.type)) {
        return false;
      }
      return true;
    });
  }

  get sortedRows() {
    return sortRows(this.filteredRows, this.sortBy, this.sortDescending);
  }

  get regionOptions() {
    return this.regions.map((r) => ({ key: r, label: r }));
  }

  get statusOptions() {
    return ['pending', 'running', 'dead'].map((s) => ({ key: s, label: s }));
  }

  get typeOptions() {
    return [
      'batch',
      'parameterized',
      'periodic',
      'service',
      'system',
      'sysbatch',
    ].map((t) => ({ key: t, label: t }));
  }

  get failedRegions() {
    return Object.entries(this.errors).map(([region, message]) => ({
      region,
      message,
    }));
  }

  async loadRegion(region) {
    const url = `/v1/jobs?meta=true&namespace=${encodeURIComponent(
      this.namespace
    )}&region=${encodeURIComponent(region)}`;
    try {
      const response = await this.token.authorizedRawRequest(url);
      if (!response.ok) {
        throw new Error(
          response.status === 403
            ? 'Your token cannot list the jobs of this region.'
            : `The jobs could not be loaded (${response.status}).`
        );
      }
      const stubs = (await response.json()) || [];
      if (this.isDestroying || this.isDestroyed) return;
      this.byRegion = { ...this.byRegion, [region]: stubs };
      const { [region]: _, ...rest } = this.errors; // eslint-disable-line no-unused-vars
      this.errors = rest;
    } catch (e) {
      if (this.isDestroying || this.isDestroyed) return;
      this.errors = {
        ...this.errors,
        [region]: e.message || 'The region cannot be reached.',
      };
    }
  }

  async load() {
    await Promise.all(this.regions.map((r) => this.loadRegion(r)));
    if (!this.isDestroying && !this.isDestroyed) this.isLoading = false;
  }

  scheduleRefresh(region) {
    if (this.timers.has(region)) return;
    this.timers.set(
      region,
      setTimeout(() => {
        this.timers.delete(region);
        waitForPromise(this.loadRegion(region));
      }, REFRESH_DEBOUNCE_MS)
    );
  }

  live = modifier((element, [regions, namespace]) => {
    // Deferred: tracked state must not change while the modifier installs.
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribers = (regions || []).filter(Boolean).map((region) =>
      this.eventStream.subscribe({
        topics: ['Job'],
        region,
        namespace: namespace || '*',
        onEvents: () => this.scheduleRefresh(region),
        onResync: () => this.loadRegion(region).then(() => 0),
      })
    );
    let poll = null;
    if (config.environment !== 'test') {
      poll = setInterval(() => {
        const allOpen = (regions || []).every(
          (r) => this.eventStream.stateFor(r, namespace || '*') === 'open'
        );
        if (!allOpen) waitForPromise(this.load());
      }, FALLBACK_POLL_MS);
    }
    return () => {
      unsubscribers.forEach((u) => u());
      clearInterval(poll);
      this.timers.forEach((t) => clearTimeout(t));
      this.timers.clear();
    };
  });

  @action
  search(term) {
    this.searchTerm = term;
    this.page = 1;
  }

  @action
  setFilter(name, selection) {
    this[name] = selection;
    this.page = 1;
  }

  @action
  sort(key, descending) {
    this.sortBy = key;
    this.sortDescending = descending;
  }

  @action
  setPage(page) {
    this.page = page;
  }

  @action
  setPageSize(size) {
    this.pageSize = size;
    this.page = 1;
  }

  get viewState() {
    return {
      searchTerm: this.searchTerm,
      selectionRegion: this.selectionRegion,
      selectionStatus: this.selectionStatus,
      selectionType: this.selectionType,
      sortBy: this.sortBy,
      sortDescending: this.sortDescending,
    };
  }

  @action
  applyView(state) {
    [
      'searchTerm',
      'selectionRegion',
      'selectionStatus',
      'selectionType',
      'sortBy',
      'sortDescending',
    ].forEach((key) => {
      if (key in state) this[key] = state[key];
    });
    this.page = 1;
  }

  // Opens the job in its region: the region query parameter switches the
  // active region of the UI.
  @action
  gotoJob(row) {
    this.router.transitionTo('jobs.job.index', row.routeId, {
      queryParams: { region: row.region },
    });
  }
}
