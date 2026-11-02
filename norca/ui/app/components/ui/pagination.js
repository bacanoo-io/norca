/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';

const PAGE_SIZES = [10, 25, 50, 100];

/**
 *   <Ui::Pagination @page={{2}} @pageSize={{25}} @total={{120}}
 *     @onPageChange={{this.setPage}} @onPageSizeChange={{this.setPageSize}} />
 */
export default class UiPagination extends Component {
  get pageCount() {
    return Math.max(1, Math.ceil((this.args.total || 0) / this.args.pageSize));
  }

  get startsAt() {
    if (!this.args.total) return 0;
    return (this.args.page - 1) * this.args.pageSize + 1;
  }

  get endsAt() {
    return Math.min(this.args.page * this.args.pageSize, this.args.total || 0);
  }

  get hasPrevious() {
    return this.args.page > 1;
  }

  get hasNext() {
    return this.args.page < this.pageCount;
  }

  get pageSizes() {
    const sizes = new Set([...PAGE_SIZES, Number(this.args.pageSize)]);
    return [...sizes]
      .filter(Boolean)
      .sort((a, b) => a - b)
      .map((size) => ({ value: size, label: String(size) }));
  }

  // The first, the last and two pages around the current one, with gaps.
  get pageLinks() {
    const count = this.pageCount;
    const current = this.args.page;
    const pages = new Set([1, count]);
    for (let p = current - 2; p <= current + 2; p++) {
      if (p >= 1 && p <= count) pages.add(p);
    }
    const sorted = [...pages].sort((a, b) => a - b);
    const links = [];
    sorted.forEach((page, i) => {
      if (i > 0 && page - sorted[i - 1] > 1) links.push({ gap: true });
      links.push({ page, isCurrent: page === current });
    });
    return links;
  }

  @action
  goTo(page) {
    if (page < 1 || page > this.pageCount || page === this.args.page) return;
    this.args.onPageChange?.(page);
  }

  @action
  setPageSize(value) {
    this.args.onPageSizeChange?.(Number(value));
  }
}
