/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { blockAbility } from 'nomad-ui/utils/token-ability';

// Namespace quotas: read shows usage and specs, write edits them.
export default blockAbility('Quota');
