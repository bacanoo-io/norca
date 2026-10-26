/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Mirage routes and seed data of the feature areas of UI phase 3.

import registerBackupRoutes from './backups';
import seedBackups from './seeds/backups';
import registerQuotaRoutes, { seedQuotas } from './quotas';
import registerScalingRoutes, { seedScaling } from './scaling';
import registerNetworkRoutes, { seedNetwork } from './network';
import registerDeploymentRoutes from './deployments';
import registerReleaseRoutes, { seedReleases } from './releases';
import registerObjectRoutes, { seedObjects } from './objects';
import registerSecretRoutes, { seedSecrets } from './secrets';

export function registerNorcaAreas(server) {
  registerBackupRoutes(server);
  registerQuotaRoutes(server);
  registerScalingRoutes(server);
  registerNetworkRoutes(server);
  registerDeploymentRoutes(server);
  registerReleaseRoutes(server);
  registerObjectRoutes(server);
  registerSecretRoutes(server);
}

// Development data (the default scenario).
export function seedNorcaAreas(server) {
  seedBackups(server);
  seedQuotas(server);
  seedScaling(server);
  seedNetwork(server);
  seedReleases(server);
  seedObjects(server);
  seedSecrets(server);
}
