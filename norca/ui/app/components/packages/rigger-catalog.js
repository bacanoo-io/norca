/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { debounce } from '@ember/runloop';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  CATEGORIES,
  OLD_INDEX_DAYS,
  categoryIcon,
  categoryLabel,
  indexAgeDays,
  listRiggerRegistries,
  riggerBackend,
  riggerStackBackend,
  riggerStackDetail,
  riggerModuleDetail,
  riggerModuleIcon,
  searchRiggerRegistry,
} from 'nomad-ui/utils/rigger';

// How long a typed search waits for the next key.
const SEARCH_DELAY = 250;

/**
 * The catalog of Rigger (design/rig-integration.md 4.8): the modules of one
 * registry at its last good sync, searched on the servers by text,
 * category and keyword, a page at a time. A registry that shows only what
 * is selected shows that first, the whole index on request. A module opens
 * in a panel with its README and the install form.
 */
export default class PackagesRiggerCatalog extends Component {
  @service token;
  @service system;
  @service router;

  @tracked registries = [];
  @tracked registryName = '';
  @tracked isLoadingRegistries = true;
  @tracked registriesError = null;

  // module or stack: what the catalog lists.
  @tracked kind = 'module';
  @tracked text = '';
  @tracked category = '';
  @tracked keyword = '';
  @tracked showAll = false;
  @tracked page = 1;
  @tracked pageSize = 24;

  @tracked items = [];
  @tracked total = 0;
  @tracked isSearching = false;
  @tracked searchError = null;
  @tracked notSynced = false;

  @tracked selected = null;
  @tracked detail = null;
  @tracked detailError = null;
  @tracked iconURL = null;
  @tracked panelTab = 'install';

  setup = modifier(() => {
    this.loadRegistries();
    return () => this.releaseIcon();
  });

  // Registries are objects of a region: that of the UI.
  get region() {
    return this.system.activeRegion || '';
  }

  get registry() {
    return this.registries.find((r) => r.name === this.registryName) || null;
  }

  get registryOptions() {
    return this.registries.map((r) => ({ value: r.name, label: r.name }));
  }

  get categoryOptions() {
    return [
      { value: '', label: 'All categories' },
      ...CATEGORIES.map((c) => ({ value: c, label: categoryLabel(c) })),
    ];
  }

  // A registry of display "selected" shows its selection, unless the user
  // asks for the whole index.
  get showsSelection() {
    return this.registry?.spec?.display === 'selected' && !this.showAll;
  }

  get indexAge() {
    return indexAgeDays(this.registry);
  }

  get isOld() {
    return this.indexAge !== null && this.indexAge > OLD_INDEX_DAYS;
  }

  get lastSync() {
    const s = this.registry?.status || {};
    return { at: s.lastGoodSyncAt, commit: (s.commit || '').slice(0, 12) };
  }

  // The cards: a page of the search, or the selection filtered here.
  get cards() {
    const list = this.showsSelection ? this.selectionPage : this.items;
    return list.map((item) => this.card(item));
  }

  get selection() {
    const text = this.text.trim().toLowerCase();
    return (this.registry?.status?.shown || []).filter((item) => {
      if (item.kind !== this.kind) return false;
      if (this.category && item.category !== this.category) return false;
      if (this.keyword && !(item.keywords || []).includes(this.keyword))
        return false;
      if (!text) return true;
      return [`${item.namespace || ''}/${item.name}`, item.description || '']
        .join(' ')
        .toLowerCase()
        .includes(text);
    });
  }

  get selectionPage() {
    const start = (this.page - 1) * this.pageSize;
    return this.selection.slice(start, start + this.pageSize);
  }

  get shownTotal() {
    return this.showsSelection ? this.selection.length : this.total;
  }

  card(item) {
    const address = item.namespace
      ? `${item.namespace}/${item.name}`
      : item.name;
    return {
      ...item,
      address,
      key: `${this.registryName}/${address}`,
      newest: (item.versions || [])[0] || '',
      icon: categoryIcon(item.category),
      categoryText: categoryLabel(item.category),
    };
  }

  async loadRegistries() {
    this.isLoadingRegistries = true;
    this.registriesError = null;
    try {
      const r = await waitForPromise(
        listRiggerRegistries(this.token, this.region)
      );
      this.registries = r.body.sort((a, b) => a.name.localeCompare(b.name));
      if (!this.registry) {
        this.registryName =
          (
            this.registries.find((g) => g.name === 'global') ||
            this.registries[0]
          )?.name || '';
      }
      await this.search();
    } catch (e) {
      this.registriesError = errorMessage(e);
    } finally {
      this.isLoadingRegistries = false;
    }
  }

  async search() {
    this.searchError = null;
    this.notSynced = false;
    if (!this.registry || this.showsSelection) {
      this.items = [];
      this.total = 0;
      return;
    }
    this.isSearching = true;
    try {
      const r = await waitForPromise(
        searchRiggerRegistry(this.token, this.region, this.registryName, {
          kind: this.kind,
          text: this.text.trim(),
          category: this.category,
          keyword: this.keyword.trim(),
          offset: (this.page - 1) * this.pageSize,
          limit: this.pageSize,
        })
      );
      this.items = r.body?.Items || [];
      this.total = r.body?.Total || 0;
    } catch (e) {
      this.items = [];
      this.total = 0;
      if (e?.status === 409) this.notSynced = true;
      else this.searchError = errorMessage(e);
    } finally {
      this.isSearching = false;
    }
  }

  // A new query starts at its first page.
  refresh() {
    this.page = 1;
    this.search();
  }

  @action setRegistry(name) {
    this.registryName = name;
    this.showAll = false;
    this.refresh();
  }

  @action setText(value) {
    this.text = value;
    debounce(this, this.refresh, SEARCH_DELAY);
  }

  @action setKeyword(value) {
    this.keyword = value;
    debounce(this, this.refresh, SEARCH_DELAY);
  }

  @action setCategory(value) {
    this.category = value;
    this.refresh();
  }

  @action setShowAll(value) {
    this.showAll = value;
    this.refresh();
  }

  @action setPage(page) {
    this.page = page;
    this.search();
  }

  @action setPageSize(size) {
    this.pageSize = Number(size);
    this.refresh();
  }

  get kindOptions() {
    return [
      { value: 'module', label: 'Modules' },
      { value: 'stack', label: 'Stacks' },
    ];
  }

  get isStacks() {
    return this.kind === 'stack';
  }

  @action setKind(kind) {
    this.kind = kind;
    this.refresh();
  }

  get backend() {
    if (!this.selected) return null;
    if (this.selected.kind === 'stack') {
      return riggerStackBackend({
        token: this.token,
        registry: this.registryName,
        stack: this.selected.address,
      });
    }
    return riggerBackend({
      token: this.token,
      registry: this.registryName,
      module: this.selected.address,
    });
  }

  @action async open(card, tab) {
    this.releaseIcon();
    this.selected = card;
    this.panelTab = tab;
    this.detail = null;
    this.detailError = null;
    try {
      const detail =
        card.kind === 'stack' ? riggerStackDetail : riggerModuleDetail;
      const r = await waitForPromise(
        detail(
          this.token,
          this.region,
          this.registryName,
          card.address,
          card.newest
        )
      );
      if (this.selected !== card) return;
      this.detail = r.body;
      // The icon of the module, fetched only for the module opened.
      if (r.body?.Icon) {
        const url = await waitForPromise(
          riggerModuleIcon(
            this.token,
            this.region,
            this.registryName,
            card.address,
            card.newest
          )
        );
        if (this.selected === card) this.iconURL = url;
        else if (url) URL.revokeObjectURL(url);
      }
    } catch (e) {
      if (this.selected === card) this.detailError = errorMessage(e);
    }
  }

  releaseIcon() {
    if (this.iconURL) URL.revokeObjectURL(this.iconURL);
    this.iconURL = null;
  }

  @action close() {
    this.releaseIcon();
    this.selected = null;
    this.detail = null;
  }

  @action setPanelTab(tab) {
    this.panelTab = tab;
  }

  @action deployed(response, release) {
    this.close();
    // An apply of a stack runs as an operation, followed on the stack page.
    if (response?.Operation) {
      this.router.transitionTo(
        'packages.stack',
        release.namespace,
        response.Stack,
        {
          queryParams: {
            region: release.region || null,
            operation: response.Operation,
          },
        }
      );
      return;
    }
    this.router.transitionTo(
      'packages.release',
      release.namespace,
      release.name,
      {
        queryParams: { region: release.region || null },
      }
    );
  }
}
