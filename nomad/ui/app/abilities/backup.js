/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { blockAbility } from 'nomad-ui/utils/token-ability';
import { isManagement } from 'nomad-ui/utils/norca-acl';

// Backups: read lists, browses, compares and restores selectively (with the
// namespace permissions as well); write takes, pins, labels, deletes and
// links downloads. A full restore needs a management token, approved by a
// second one.
export default blockAbility('Backup', { restoreFull: isManagement });
