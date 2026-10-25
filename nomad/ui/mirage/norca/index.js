/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Mirage routes and seed data of the feature areas of UI phase 3.

import registerBackupRoutes from './backups';
import seedBackups from './seeds/backups';
import registerQuotaRoutes, { seedQuotas } from './quotas';
import registerScalingRoutes, { seedScaling } from './scaling';

export function registerNorcaAreas(server) {
  registerBackupRoutes(server);
  registerQuotaRoutes(server);
  registerScalingRoutes(server);
}

// Development data (the default scenario).
export function seedNorcaAreas(server) {
  seedBackups(server);
  seedQuotas(server);
  seedScaling(server);
}
