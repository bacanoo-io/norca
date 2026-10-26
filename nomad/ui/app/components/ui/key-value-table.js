/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

// Flattens a nested object into heading rows (the nested objects) and
// key/value rows, in key order, keeping the path of each key.
function flatten(object, prefix = '', rows = []) {
  Object.keys(object || {})
    .sort()
    .forEach((key) => {
      const value = object[key];
      const path = prefix ? `${prefix}.${key}` : key;
      if (isObject(value)) {
        rows.push({ isHeading: true, prefix, key, path });
        flatten(value, path, rows);
      } else {
        rows.push({ isHeading: false, prefix, key, path, value });
      }
    });
  return rows;
}

/**
 * A table of key/value pairs (meta, attributes), nested objects grouped
 * under heading rows with their path. With @editable each value can be
 * edited or deleted: @onKVSave({ key, value }) receives the full key and
 * the new value (null deletes).
 *
 *   <Ui::KeyValueTable @data={{@job.meta.structured}} @caption="Job meta" />
 */
export default class UiKeyValueTable extends Component {
  @tracked editingPath = null;
  @tracked draft = '';

  get rows() {
    return flatten(this.args.data);
  }

  @action
  edit(row) {
    this.editingPath = row.path;
    this.draft = row.value ?? '';
  }

  @action
  setDraft(value) {
    this.draft = value;
  }

  @action
  cancel() {
    this.editingPath = null;
  }

  @action
  keydown(event) {
    if (event.key === 'Escape') this.cancel();
  }

  @action
  save(row, event) {
    event?.preventDefault();
    this.args.onKVSave?.({ key: row.path, value: this.draft });
    this.editingPath = null;
  }

  @action
  delete(row) {
    this.args.onKVSave?.({ key: row.path, value: null });
    this.editingPath = null;
  }
}
