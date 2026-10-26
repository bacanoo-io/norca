/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from './abstract';
import { allowsBlock, isManagement } from 'nomad-ui/utils/norca-acl';

// The backup ACL block: read (list, details, browse, compare, selective
// restore together with the namespace permissions), write (manual backup,
// pin, labels, delete, download link). A full restore needs a management
// token, approved by a second one.
export default class Backup extends AbstractAbility {
  get canRead() {
    return allowsBlock(this.token, 'Backup', 'read');
  }

  get canWrite() {
    return allowsBlock(this.token, 'Backup', 'write');
  }

  get canRestoreFull() {
    return isManagement(this.token);
  }
}
