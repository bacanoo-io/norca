/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from './abstract';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';

// The admission ACL block: read (policies), write (create, change, delete).
export default class Admission extends AbstractAbility {
  get canRead() {
    return allowsBlock(this.token, 'Admission', 'read');
  }

  get canWrite() {
    return allowsBlock(this.token, 'Admission', 'write');
  }
}
