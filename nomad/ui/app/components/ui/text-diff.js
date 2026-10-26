/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { cached } from '@glimmer/tracking';
import { diffHunks } from 'nomad-ui/utils/text-diff';

export default class UiTextDiff extends Component {
  @cached
  get lines() {
    return diffHunks(this.args.old, this.args.new, this.args.context ?? 3);
  }

  get stats() {
    return {
      added: this.lines.filter((l) => l.type === 'add').length,
      removed: this.lines.filter((l) => l.type === 'del').length,
    };
  }

  get isUnchanged() {
    return !this.stats.added && !this.stats.removed;
  }
}
