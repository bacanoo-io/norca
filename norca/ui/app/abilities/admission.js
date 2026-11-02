/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { blockAbility } from 'nomad-ui/utils/token-ability';

// Admission policies: read lists them, write creates, changes and deletes.
export default blockAbility('Admission');
