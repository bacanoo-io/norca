/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

/**
 * A data table (Tabler card table).
 *
 *   <Ui::Table
 *     @columns={{array (hash key="name" label="Name" sortable=true)
 *                      (hash key="status" label="Status")}}
 *     @rows={{this.jobs}}
 *     @sortBy={{this.sortBy}} @sortDescending={{this.desc}} @onSort={{this.sort}}
 *     @emptyMessage="No jobs match the filter."
 *   as |row|>
 *     <td>{{row.name}}</td>
 *     <td><Ui::StatusIndicator @status={{row.status}} /></td>
 *   </Ui::Table>
 *
 * The block renders the cells of one row. @onSort(key, descending) is called
 * when a sortable header is activated; sorting itself is the caller's job.
 * @onRowClick makes rows clickable (the row also gets keyboard focus).
 */
export default class UiTable extends Component {
  get columns() {
    return (this.args.columns || []).map((column) => {
      const isSorted = column.sortable && this.args.sortBy === column.key;
      let ariaSort = null;
      if (isSorted)
        ariaSort = this.args.sortDescending ? 'descending' : 'ascending';
      return { ...column, isSorted, ariaSort };
    });
  }

  get isEmpty() {
    return !(this.args.rows && this.args.rows.length);
  }

  @action
  sort(column) {
    const descending =
      this.args.sortBy === column.key ? !this.args.sortDescending : false;
    this.args.onSort?.(column.key, descending);
  }

  @action
  rowKeydown(row, event) {
    if (event.key === 'Enter' && event.target === event.currentTarget) {
      this.args.onRowClick?.(row);
    }
  }
}
