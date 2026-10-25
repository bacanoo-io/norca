/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from './abstract';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';

// The quota ACL block: read (usage, specs), write (create, edit, delete).
export default class QuotaSpec extends AbstractAbility {
  get canRead() {
    return allowsBlock(this.token, 'Quota', 'read');
  }

  get canWrite() {
    return allowsBlock(this.token, 'Quota', 'write');
  }
}
