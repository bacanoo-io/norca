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
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { listCatalogPacks, getCatalogPack } from 'nomad-ui/utils/releases';
import { compareVersionsDesc } from 'nomad-ui/utils/pack-form';

/**
 * The packs of the cluster registries (GET /v1/packs), one card per pack
 * with its newest version, and the install panel: the form the servers
 * synthesize from the variables of the pack, and its README.
 */
export default class PackagesCatalog extends Component {
  @service token;
  @service system;
  @service router;

  @tracked entries = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked searchTerm = '';
  @tracked category = '';
  @tracked registry = '';

  @tracked selected = null;
  @tracked detail = null;
  @tracked detailError = null;
  @tracked panelTab = 'install';

  setup = modifier(() => {
    this.load();
  });

  // Registries are objects of a region: that of the UI.
  get region() {
    return this.system.activeRegion || '';
  }

  // One per registry and pack, with the versions newest first.
  get packs() {
    const byPack = new Map();
    for (const e of this.entries) {
      const key = `${e.Registry}/${e.Name}`;
      if (!byPack.has(key)) byPack.set(key, []);
      byPack.get(key).push(e);
    }
    return [...byPack.values()]
      .map((versions) => {
        versions.sort((a, b) => compareVersionsDesc(a.Version, b.Version));
        return {
          ...versions[0],
          key: `${versions[0].Registry}/${versions[0].Name}`,
          versionCount: versions.length,
        };
      })
      .sort(
        (a, b) =>
          a.Name.localeCompare(b.Name) || a.Registry.localeCompare(b.Registry)
      );
  }

  get categories() {
    return [
      ...new Set(this.packs.map((p) => p.Category).filter(Boolean)),
    ].sort();
  }

  get registries() {
    return [...new Set(this.packs.map((p) => p.Registry))].sort();
  }

  get categoryOptions() {
    return [
      { value: '', label: 'All categories' },
      ...this.categories.map((c) => ({ value: c, label: c })),
    ];
  }

  get registryOptions() {
    return [
      { value: '', label: 'All registries' },
      ...this.registries.map((r) => ({ value: r, label: r })),
    ];
  }

  get filteredPacks() {
    const term = this.searchTerm.toLowerCase().trim();
    return this.packs.filter((p) => {
      if (this.category && p.Category !== this.category) return false;
      if (this.registry && p.Registry !== this.registry) return false;
      if (!term) return true;
      return [p.Name, p.Description, p.Registry, p.Category].some((s) =>
        String(s || '')
          .toLowerCase()
          .includes(term)
      );
    });
  }

  @action setSearch(value) {
    this.searchTerm = value;
  }

  @action setCategory(value) {
    this.category = value;
  }

  @action setRegistry(value) {
    this.registry = value;
  }

  async load() {
    this.isLoading = true;
    this.loadError = null;
    try {
      const r = await waitForPromise(listCatalogPacks(this.token, this.region));
      this.entries = r.body || [];
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action async open(pack, tab) {
    this.selected = pack;
    this.panelTab = tab;
    this.detail = null;
    this.detailError = null;
    try {
      const r = await waitForPromise(
        getCatalogPack(
          this.token,
          this.region,
          pack.Registry,
          pack.Name,
          pack.Version
        )
      );
      if (this.selected === pack) this.detail = r.body;
    } catch (e) {
      if (this.selected === pack) this.detailError = errorMessage(e);
    }
  }

  @action close() {
    this.selected = null;
    this.detail = null;
  }

  @action setPanelTab(tab) {
    this.panelTab = tab;
  }

  @action deployed(response, release) {
    this.close();
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
