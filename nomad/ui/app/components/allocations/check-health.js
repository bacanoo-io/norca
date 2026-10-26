/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class AllocationsCheckHealth extends Component {
  get data() {
    const status = this.args.status || {};
    return [
      { key: 'queued', label: 'Pending', value: status.pending || 0 },
      { key: 'failed', label: 'Failing', value: status.failure || 0 },
      { key: 'running', label: 'Success', value: status.success || 0 },
    ];
  }
}
