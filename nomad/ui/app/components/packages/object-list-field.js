/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { cellId, newRow, setCell } from 'nomad-ui/utils/pack-form';

/**
 * A list of objects of the pack install form as a table (the object-list
 * widget): a row per item, a column per attribute of the items, rows added
 * and removed. Every change sends the whole list to @onChange.
 *
 *   <Packages::ObjectListField @field={{f}} @value={{v}} @errors={{errors}}
 *     @onChange={{this.setValue}} />
 *
 * The rows are rendered from stable references by index, so a cell keeps
 * its focus while the list is replaced on every keystroke.
 */
export default class PackagesObjectListField extends Component {
  refs = [];

  get field() {
    return this.args.field;
  }

  get items() {
    return Array.isArray(this.args.value) ? this.args.value : [];
  }

  get rows() {
    const n = this.items.length;
    while (this.refs.length < n) this.refs.push({ index: this.refs.length });
    return this.refs.slice(0, n);
  }

  get columns() {
    return [
      ...this.field.columns.map((c) => ({
        key: c.key,
        label: c.required ? `${c.title} *` : c.title,
      })),
      { key: '_actions', label: '', class: 'w-1' },
    ];
  }

  get error() {
    return this.args.errors?.[this.field.id];
  }

  get canAdd() {
    const max = this.field.maxItems;
    return max === undefined || this.items.length < max;
  }

  cell = (index, column) => {
    const v = this.items[index]?.[column.key];
    if (column.json && v !== undefined && typeof v !== 'string') {
      return JSON.stringify(v);
    }
    return v ?? '';
  };

  cellError = (index, column) => {
    return this.args.errors?.[cellId(this.field, index, column)];
  };

  options = (column) => {
    const list = (column.enum || []).map(String);
    if (!column.required) list.unshift('');
    return list.map((v) => ({
      value: v,
      label: v === '' ? '(default)' : column.enumLabels[v] || v,
    }));
  };

  @action change(index, column, value) {
    const items = [...this.items];
    items[index] = setCell(this.field, items[index], column, value);
    this.args.onChange(this.field, items);
  }

  @action add() {
    this.args.onChange(this.field, [...this.items, newRow(this.field)]);
  }

  @action remove(index) {
    this.args.onChange(
      this.field,
      this.items.filter((_, i) => i !== index)
    );
  }
}
