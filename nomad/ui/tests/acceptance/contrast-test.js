/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import { seedNorcaAreas } from 'nomad-ui/mirage/norca';
import {
  seedDeployments,
  MULTIREGION,
} from 'nomad-ui/mirage/norca/deployments';
import { useToken } from 'nomad-ui/tests/helpers/norca';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

// WCAG 2.1 AA contrast of the pages of the new feature areas in the light
// and the dark theme (acceptance criteria of the UI plan). The other a11y
// audits of the test suite leave the color-contrast rule out.
const PAGES = [
  '/backups',
  '/quotas',
  '/quotas/team-a',
  '/scaling',
  '/scaling/policy-api-web?namespace=default',
  '/services',
  '/services/api?namespace=default',
  '/network',
  '/network?tab=policies',
  '/network/policies/default/db-ingress',
  '/packages',
  '/packages/platform/postgres-operator',
  '/objects',
  '/objects/db.example.com/PostgresCluster/reports?namespace=default',
  '/kinds',
  '/kinds/db.example.com/PostgresCluster',
  '/admission',
  '/admission/no-privileged-containers',
  '/deployments?region=eu-west',
];

module('Acceptance | contrast of the feature area pages', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  ['light', 'dark'].forEach((theme) => {
    test(`every page meets the AA contrast in the ${theme} theme`, async function (assert) {
      window.localStorage.clear();
      window.localStorage.norcaTheme = theme;
      server.create('agent');
      server.create('node-pool');
      server.create('node');
      server.create('job', {
        id: 'api',
        createAllocations: false,
        noDeployments: true,
        shallow: true,
        multiregion: MULTIREGION,
      });
      seedNorcaAreas(server);
      seedDeployments(server);
      useToken(null, 'management');

      const problems = [];
      const pages = [...PAGES, `/backups/${server.db.norcaBackups[0].ID}`];
      for (const page of pages) {
        await visit(page);
        const result = await window.axe.run(
          document.querySelector('#main-content'),
          { runOnly: { type: 'rule', values: ['color-contrast'] } }
        );
        for (const v of result.violations)
          for (const node of v.nodes)
            problems.push(
              `${page}: ${node.target.join(' ')}: ${node.any[0]?.message}`
            );
      }
      assert.deepEqual(problems, [], `${theme}: no contrast violations`);
      // and the other rules on the last page
      await a11yAudit(assert, STRICT);
    });
  });
});
