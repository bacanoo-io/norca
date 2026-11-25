/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, select, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedReleases } from 'nomad-ui/mirage/norca/releases';
import { seedRigger, seedRiggerStack } from 'nomad-ui/mirage/norca/rigger';
import { useToken, lastBody, lastRequest } from 'nomad-ui/tests/helpers/norca';

const SOURCE = 'https://git.example.com/acme/rigger-modules//stacks/site';

module('Acceptance | rigger stacks', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    seedReleases(server);
    seedRigger(server);
    useToken(null, 'management');
  });

  test('a published stack is applied from the catalog and followed on its page', async function (assert) {
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await select('[data-test-catalog-registry]', 'acme');
    await select('[data-test-catalog-kind]', 'stack');

    assert.ok(
      lastRequest('GET', '/rigger/registry/acme/search').url.includes(
        'kind=stack'
      ),
      'the servers list the stacks'
    );
    assert.dom('[data-test-module-card="acme/site"]').exists();
    assert.dom('[data-test-module-card="acme/web-app"]').doesNotExist();
    assert.dom('[data-test-btn-install="acme/site"]').hasText('Apply');

    await click('[data-test-btn-install="acme/site"]');
    assert.ok(lastRequest('GET', '/rigger/registry/acme/stack'));
    assert
      .dom('[data-test-form-release-name]')
      .doesNotExist('a stack names its releases itself');

    await click('[data-test-btn-deploy]');
    assert.notOk(lastRequest('PUT', '/v1/rigger/stack/apply'));
    assert
      .dom('[data-test-pack-field="domain"] [data-test-field-error]')
      .hasText('Domain is required');
    await fillIn('[data-test-pack-field="domain"] input', 'shop.example.com');

    await click('[data-test-tab="cli"]');
    const command = document.querySelector(
      '[data-test-form-cli-command] pre'
    ).textContent;
    assert.ok(command.includes('/v1/rigger/stack/apply?namespace=default'));
    assert.ok(command.includes('shop.example.com'));

    await click('[data-test-btn-plan]');
    assert.strictEqual(lastBody('PUT', '/v1/rigger/stack/plan').Source, SOURCE);
    assert
      .dom('[data-test-plan-result] [data-test-plan-action="install"]')
      .exists({ count: 2 }, 'a release per part');
    assert.dom('[data-test-plan-result]').includesText('default/site-web');

    await click('[data-test-btn-deploy]');
    const body = lastBody('PUT', '/v1/rigger/stack/apply');
    assert.strictEqual(body.Source, SOURCE);
    assert.strictEqual(body.Version, '1.1.0');
    assert.deepEqual(
      body.ValuesJSON,
      { domain: 'shop.example.com' },
      'a default is left to the stack'
    );
    assert.ok(
      currentURL().startsWith('/packages/stack/default/site'),
      'the page of the stack'
    );
    assert.ok(currentURL().includes('operation=op-site-1'));

    assert.dom('[data-test-operation-phase]').includesText('Succeeded');
    assert.dom('[data-test-operation-log]').includesText('release.install');
    assert
      .dom('[data-test-operation-outcomes]')
      .includesText('default/site-db: install (revision 1)');
    assert.dom('[data-test-stack-release="site-db"]').exists();
    assert.dom('[data-test-stack-release="site-web"]').includesText('part web');
    assert.dom('[data-test-stack-source]').hasText(SOURCE);
    await a11yAudit(assert, STRICT);

    await click('[data-test-stack-release="site-web"] a');
    assert.strictEqual(currentURL(), '/packages/default/site-web');
    await click('[data-test-release-owner-link]');
    assert.ok(currentURL().startsWith('/packages/stack/default/site'));
    assert.dom('[data-test-stack-release="site-db"]').exists();
  });

  test('the stacks of a namespace, and a stack checked, applied again and destroyed', async function (assert) {
    seedRiggerStack(server);
    await visit('/packages');
    await click('[data-test-tab="stacks"]');
    assert.dom('[data-test-stack-row="default/site"]').exists();
    assert.dom('[data-test-stack-row="default/site"]').hasText('site default');
    assert.dom('[data-test-stack-last-operation]').hasText('apply: Succeeded');

    await click('[data-test-stack-row="default/site"] a');
    assert.strictEqual(currentURL(), '/packages/stack/default/site');
    assert.ok(
      lastRequest('GET', '/v1/releases').url.includes(
        'owner=stack%2Fdefault%2Fsite'
      ),
      'the releases the stack owns'
    );
    assert.dom('[data-test-stack-operation="op-site-1"]').exists();
    assert.dom('[data-test-operation-log]').includesText('stack.apply done');

    await click('[data-test-stack-status]');
    assert.deepEqual(lastBody('PUT', '/v1/rigger/stack/status'), {
      Source: SOURCE,
      Version: '1.0.0',
    });
    assert
      .dom('[data-test-stack-releases]')
      .includesText('1 drifted')
      .includesText('in sync');

    await click('[data-test-stack-apply]');
    assert.dom('[data-test-form-namespace]').isDisabled();
    await fillIn('[data-test-pack-field="domain"] input', 'www.example.com');
    await click('[data-test-btn-deploy]');
    assert.strictEqual(
      lastBody('PUT', '/v1/rigger/stack/apply').Version,
      '1.0.0',
      'the version the stack runs'
    );
    assert.dom('[data-test-stack-operation="op-site-2"]').exists();
    assert.dom('[data-test-stack-operation-detail]').includesText('op-site-2');

    await click('[data-test-stack-destroy]');
    await fillIn('[data-test-confirm-typed]', 'site');
    await click('[data-test-confirm-submit]');
    assert.deepEqual(lastBody('PUT', '/v1/rigger/stack/destroy'), {
      Source: SOURCE,
      Version: '1.0.0',
    });
    assert.dom('[data-test-stack-operation-detail]').includesText('destroy');
    assert
      .dom('[data-test-operation-outcomes]')
      .includesText('default/site-web: uninstall');
  });

  test('a failed operation names the release it failed at', async function (assert) {
    seedRiggerStack(server);
    const op = server.db.norcaObjects.findBy({ Name: 'op-site-1' });
    server.db.norcaObjects.update(op.id, {
      Status: {
        ...op.Status,
        phase: 'Failed',
        outcomes: [
          {
            namespace: 'default',
            release: 'site-db',
            action: 'install',
            failed: true,
          },
          {
            namespace: 'default',
            release: 'site-web',
            action: 'install',
            skipped: true,
          },
        ],
      },
    });
    await visit('/packages/stack/default/site?operation=op-site-1');
    assert.dom('[data-test-operation-phase]').includesText('Failed');
    assert
      .dom('[data-test-operation-outcomes]')
      .includesText('default/site-db: install (failed)');
    assert
      .dom('[data-test-operation-outcomes]')
      .includesText('default/site-web: install (skipped)');
  });

  test('an operation of the past is shown when chosen, a running one can be cancelled', async function (assert) {
    seedRiggerStack(server);
    const op = server.db.norcaObjects.findBy({ Name: 'op-site-1' });
    server.db.norcaObjects.update(op.id, {
      Status: { ...op.Status, phase: 'Running', outcomes: [] },
    });
    await visit('/packages/stack/default/site?operation=op-site-1');
    assert.dom('[data-test-operation-phase]').includesText('Running');
    await click('[data-test-operation-cancel]');
    assert.ok(lastRequest('PUT', '/v1/rigger/operation/op-site-1/cancel'));
    assert.dom('[data-test-operation-phase]').includesText('Cancelled');
  });
});
