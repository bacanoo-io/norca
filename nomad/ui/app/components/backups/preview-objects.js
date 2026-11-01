/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

/**
 * The objects a full restore changes, a page at a time.
 *
 *   <Backups::PreviewObjects @preview={{this.preview}} />
 */
export default class BackupsPreviewObjects extends Component {
  @tracked requestedPage = 1;
  @tracked pageSize = 25;

  get objects() {
    return this.args.preview?.Objects || [];
  }

  get page() {
    const pages = Math.max(1, Math.ceil(this.objects.length / this.pageSize));
    return Math.min(this.requestedPage, pages);
  }

  get rows() {
    const first = (this.page - 1) * this.pageSize;
    return this.objects.slice(first, first + this.pageSize);
  }

  @action setPage(page) {
    this.requestedPage = page;
  }

  @action setPageSize(size) {
    this.pageSize = Number(size);
    this.requestedPage = 1;
  }
}
