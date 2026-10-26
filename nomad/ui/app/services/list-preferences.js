/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service from '@ember/service';
import { tracked } from '@glimmer/tracking';

const PREFIX = 'norcaList.';

function read(id) {
  try {
    const raw = window.localStorage.getItem(PREFIX + id);
    const parsed = raw ? JSON.parse(raw) : null;
    return parsed && typeof parsed === 'object' ? parsed : {};
  } catch (e) {
    return {};
  }
}

function write(id, value) {
  try {
    window.localStorage.setItem(PREFIX + id, JSON.stringify(value));
  } catch (e) {
    // storage unavailable (private mode): the preference lasts for the session
  }
}

/**
 * Preferences of the unified lists (Ui::DataTable), per list id, kept in
 * localStorage: the columns the user chose to show or hide and the saved
 * views (a name and the state of the list: filters, sort, columns).
 *
 *   prefs.columnsFor('jobs')             -> { shown: [...], hidden: [...] } or null
 *   prefs.setColumns('jobs', { shown, hidden })
 *   prefs.viewsFor('jobs')               -> [{ name, state }]
 *   prefs.saveView('jobs', 'Failed batch', { status: ['failed'], ... })
 *   prefs.deleteView('jobs', 'Failed batch')
 */
export default class ListPreferencesService extends Service {
  // Bumped on every write so that getters depending on the storage
  // recompute.
  @tracked revision = 0;

  cache = new Map();

  load(id) {
    // eslint-disable-next-line no-unused-expressions
    this.revision;
    if (!this.cache.has(id)) this.cache.set(id, read(id));
    return this.cache.get(id);
  }

  store(id, value) {
    this.cache.set(id, value);
    write(id, value);
    this.revision++;
  }

  columnsFor(id) {
    const prefs = this.load(id);
    return prefs.columns || null;
  }

  setColumns(id, columns) {
    this.store(id, { ...this.load(id), columns });
  }

  resetColumns(id) {
    const { columns, ...rest } = this.load(id); // eslint-disable-line no-unused-vars
    this.store(id, rest);
  }

  viewsFor(id) {
    const prefs = this.load(id);
    return Array.isArray(prefs.views) ? prefs.views : [];
  }

  saveView(id, name, state) {
    const views = this.viewsFor(id).filter((view) => view.name !== name);
    views.push({ name, state });
    views.sort((a, b) => a.name.localeCompare(b.name));
    this.store(id, { ...this.load(id), views });
  }

  deleteView(id, name) {
    const views = this.viewsFor(id).filter((view) => view.name !== name);
    this.store(id, { ...this.load(id), views });
  }

  // Tests start from a clean slate.
  clearAll() {
    this.cache.clear();
    try {
      Object.keys(window.localStorage)
        .filter((key) => key.startsWith(PREFIX))
        .forEach((key) => window.localStorage.removeItem(key));
    } catch (e) {
      // ignore
    }
    this.revision++;
  }
}
