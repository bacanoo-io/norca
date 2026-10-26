/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { sortRows } from 'nomad-ui/utils/sort-rows';

export default class JobsRecentAllocations extends Component {
  get recentAllocations() {
    return sortRows(this.args.job.allocations || [], 'modifyIndex', true).slice(
      0,
      5
    );
  }
}
