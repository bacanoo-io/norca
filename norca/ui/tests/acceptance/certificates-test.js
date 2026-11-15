/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedCertificates } from 'nomad-ui/mirage/norca/certificates';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

const WEB = '/objects/norca.io/Certificate/web-tls?namespace=default';

module('Acceptance | certificates', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    server.create('namespace', { id: 'staging' });
    seedCertificates(server);
    useToken(null, 'management');
  });

  test('the page lists the certificates by expiry, the issuers, the open orders and the issuing CAs', async function (assert) {
    await visit('/certificates');

    assert.dom('[data-test-page-title]').hasText('Certificates');
    assert.dom('[data-test-gutter-link="certificates"]').exists();
    assert
      .dom('[data-test-stat="certificates"] [data-test-stat-value]')
      .hasText('3');
    assert
      .dom('[data-test-stat="problems"] [data-test-stat-value]')
      .hasText('1');
    assert
      .dom('[data-test-stat="waiting"] [data-test-stat-value]')
      .hasText('1');
    assert
      .dom('[data-test-stat="issuers"] [data-test-stat-value]')
      .hasText('2');

    const names = [
      ...document.querySelectorAll(
        '[data-test-certificate-list] [data-test-certificate-link]'
      ),
    ].map((a) => a.textContent.trim());
    assert.deepEqual(
      names,
      ['api-tls', 'web-tls', 'shop-tls'],
      'the soonest expiry first, a certificate that is not issued last'
    );
    assert
      .dom('[data-test-certificate-expires="api-tls"]')
      .includesText('expires in 5 days');
    assert
      .dom('[data-test-certificate-expires="shop-tls"]')
      .hasText('not issued');
    assert.dom('[data-test-certificate-row-state="web-tls"]').hasText('ready');
    assert.dom('[data-test-certificate-row-state="api-tls"]').hasText('failed');
    assert
      .dom('[data-test-certificate-row-state="shop-tls"]')
      .hasText('issuing');

    assert
      .dom('[data-test-issuer-list] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert
      .dom('[data-test-issuer-list]')
      .includesText('controller norca-acme, heartbeat');
    assert
      .dom('[data-test-order-list] [data-test-ui-table-row]')
      .exists({ count: 1 });
    assert.dom('[data-test-order-list]').includesText('processing');

    assert
      .dom('[data-test-authorities] [data-test-ui-table-row]')
      .exists({ count: 2 });
    const rows = [
      ...document.querySelectorAll(
        '[data-test-authorities] [data-test-ui-table-row]'
      ),
    ].map((r) => r.textContent.replace(/\s+/g, ' ').trim());
    assert.ok(rows[0].startsWith('a1b2c3d4 active'), rows[0]);
    assert.ok(rows[1].startsWith('e5f6a7b8 retired'), rows[1]);
    assert.ok(rows[1].includes('imported'), rows[1]);

    const download = document.querySelector('[data-test-download-roots]');
    assert.ok(
      /^norca-certificate-roots.*\.pem$/.test(
        download.getAttribute('download')
      ),
      download.getAttribute('download')
    );
    const bundle = decodeURIComponent(download.getAttribute('href'));
    assert.strictEqual(
      bundle.match(/BEGIN CERTIFICATE/g).length,
      2,
      'the trust bundle holds the active and the retired CA'
    );
    await a11yAudit(assert, STRICT);
  });

  test('the revocation list of an issuing CA is shown on request', async function (assert) {
    await visit('/certificates');
    await click(
      '[data-test-authority-crl="e5f6a7b8-0000-4000-8000-000000000002"]'
    );

    assert.ok(
      lastRequest('GET', '/v1/certificates/crl').url.includes(
        'authority=e5f6a7b8-0000-4000-8000-000000000002'
      )
    );
    assert
      .dom('[data-test-crl] [data-test-modal-title]')
      .includesText('e5f6a7b8');
    assert.dom('[data-test-crl-pem]').includesText('BEGIN X509 CRL');
    await a11yAudit(assert, STRICT);

    await click('[data-test-crl] [data-test-modal-close]');
    assert.dom('[data-test-crl]').doesNotExist();
  });

  test('a certificate shows its names, issuer, secret and validity', async function (assert) {
    await visit('/certificates');
    await click('[data-test-certificate-link="web-tls"]');
    assert.strictEqual(currentURL(), WEB);

    assert.dom('[data-test-certificate-state]').hasText('ready');
    assert.dom('[data-test-certificate-names]').includesText('app.example.com');
    assert
      .dom('[data-test-certificate-names]')
      .includesText('*.apps.example.com');
    assert.dom('[data-test-certificate-issuer]').hasText('internal');
    assert.dom('[data-test-certificate-secret]').hasText('certs/web');
    assert.dom('[data-test-certificate-serial]').includesText('0b22');
    assert.dom('[data-test-certificate-authority]').hasText('a1b2c3d4');
    assert
      .dom('[data-test-certificate-expiry]')
      .includesText('expires in 3 months');
    assert.dom('[data-test-certificate-renewal]').includesText('in 2 months');
    assert
      .dom('[data-test-certificate-validity] [data-test-progress]')
      .hasAttribute('aria-valuetext', '11% of the validity has passed');
    assert.dom('[data-test-certificate-revoking]').doesNotExist();
    assert.dom('[data-test-certificate-retry]').doesNotExist();
    await a11yAudit(assert, STRICT);

    await click('[data-test-certificate-secret]');
    assert.strictEqual(currentURL(), '/secrets/s/certs/web?namespace=default');
    assert.dom('[data-test-secret-certificate]').includesText('web-tls');
    await click('[data-test-secret-certificate-link]');
    assert.strictEqual(currentURL(), WEB);
  });

  test('a certificate is revoked after a confirmation', async function (assert) {
    await visit(WEB);
    await click('[data-test-revoke-certificate]');
    assert
      .dom('[data-test-certificate-revoke-confirm] [data-test-confirm-body]')
      .includesText('0b22');
    await click('[data-test-confirm-submit]');

    const request = lastRequest('PUT', '/v1/certificate/web-tls/revoke');
    assert.ok(request, 'the revocation was asked for');
    assert.ok(request.url.includes('namespace=default'), request.url);
    assert.dom('[data-test-certificate-revoke-confirm]').doesNotExist();
    assert.dom('[data-test-certificate-revoking]').includesText('0b22');
    assert.dom('[data-test-certificate-state]').hasText('revoking');
    assert
      .dom('[data-test-revoke-certificate]')
      .doesNotExist('no second revocation while one is under way');

    await visit('/certificates');
    const active = document.querySelector(
      '[data-test-authorities] [data-test-ui-table-row]'
    );
    assert.strictEqual(
      active.querySelectorAll('td')[4].textContent.trim(),
      '2',
      'the active CA lists two revoked certificates'
    );
  });

  test('a failing renewal says how often it failed and shows the Failed condition as a problem', async function (assert) {
    await visit('/objects/norca.io/Certificate/api-tls?namespace=staging');

    assert.dom('[data-test-certificate-state]').hasText('failed');
    assert.dom('[data-test-certificate-retry]').includesText('3 attempts');
    assert
      .dom('[data-test-certificate-renewal]')
      .includesText('was due 25 days ago');
    assert
      .dom('[data-test-certificate-expiry]')
      .includesText('expires in 5 days');
    assert
      .dom('[data-test-certificate-expiry]')
      .hasAttribute('data-test-status-indicator', 'warning');

    const tones = Object.fromEntries(
      [
        ...document.querySelectorAll(
          '[data-test-object-conditions] [data-test-ui-table-row]'
        ),
      ].map((row) => [
        row.querySelector('td').textContent.trim(),
        row
          .querySelector('[data-test-status-indicator]')
          .getAttribute('data-test-status-indicator'),
      ])
    );
    assert.deepEqual(tones, {
      Ready: 'success',
      Renewing: 'info',
      Failed: 'danger',
    });
  });

  test('a certificate that waits for an external issuer links its open order', async function (assert) {
    await visit('/objects/norca.io/Certificate/shop-tls?namespace=default');

    assert.dom('[data-test-certificate-state]').hasText('issuing');
    assert.dom('[data-test-certificate-not-issued]').exists();
    assert.dom('[data-test-revoke-certificate]').doesNotExist();
    await click('[data-test-certificate-order]');
    assert.strictEqual(
      currentURL(),
      '/objects/norca.io/CertificateOrder/default.shop-tls.1'
    );
    assert.dom('[data-test-object-spec]').includesText('default/shop-tls');
  });

  test('a token that can only read certificates cannot revoke', async function (assert) {
    useToken({
      Namespaces: [
        {
          Name: 'default',
          Capabilities: ['list-jobs'],
          Objects: [{ Kind: 'norca.io/*', Capabilities: ['read'] }],
        },
      ],
    });
    await visit(WEB);

    assert.dom('[data-test-certificate-state]').hasText('ready');
    assert.dom('[data-test-revoke-certificate]').doesNotExist();
    assert
      .dom('[data-test-certificate-secret]')
      .doesNotExist('no link to a secret the token cannot list');
    assert.dom('[data-test-certificate]').includesText('certs/web');
  });
});
