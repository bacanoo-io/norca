/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { conditionTone } from 'nomad-ui/utils/conditions';
import { objectHealth } from 'nomad-ui/utils/objects';
import {
  authorityRow,
  certificateNames,
  certificateState,
  certificateValidity,
  orderCertificate,
} from 'nomad-ui/utils/certificates';

const DAY = 86400000;
const NOW = Date.parse('2026-11-15T12:00:00Z');
const iso = (ms) => new Date(ms).toISOString();

function certificate(status = {}, conditions = [], extra = {}) {
  return {
    Name: 'web-tls',
    Namespace: 'default',
    Generation: 1,
    Spec: { issuer: 'internal', dns_names: ['app.example.com'] },
    Status: {
      observedGeneration: 1,
      conditions: conditions.map(([type, status]) => ({ type, status })),
      ...status,
    },
    ...extra,
  };
}

const issued = (from, to, renew) => ({
  serial: '0b22',
  notBefore: iso(NOW + from * DAY),
  notAfter: iso(NOW + to * DAY),
  renewAt: iso(NOW + renew * DAY),
});

module('Unit | Utility | certificates', function () {
  test('the conditions of certificates: problems and work in progress', function (assert) {
    assert.strictEqual(conditionTone('Failed', 'True'), 'danger');
    assert.strictEqual(conditionTone('Failed', 'False'), 'success');
    assert.strictEqual(conditionTone('IssuerUnavailable', 'True'), 'warning');
    assert.strictEqual(conditionTone('IssuerUnavailable', 'False'), 'success');
    assert.strictEqual(conditionTone('Issuing', 'True'), 'info');
    assert.strictEqual(conditionTone('Issuing', 'False'), 'neutral');
    assert.strictEqual(conditionTone('Renewing', 'True'), 'info');
  });

  test('the health of a certificate object follows the polarity of its conditions', function (assert) {
    assert.deepEqual(
      objectHealth(
        certificate({}, [
          ['Ready', 'True'],
          ['Issuing', 'False'],
          ['Failed', 'False'],
        ])
      ),
      { tone: 'success', label: 'ready', behind: false }
    );
    assert.deepEqual(
      objectHealth(
        certificate({}, [
          ['Ready', 'True'],
          ['Renewing', 'True'],
          ['Failed', 'True'],
        ])
      ),
      { tone: 'danger', label: 'Failed True', behind: false }
    );
    assert.deepEqual(
      objectHealth(
        certificate({}, [
          ['Ready', 'True'],
          ['IssuerUnavailable', 'True'],
        ])
      ),
      { tone: 'warning', label: 'IssuerUnavailable True', behind: false }
    );
  });

  test('the validity of a certificate', function (assert) {
    assert.strictEqual(certificateValidity(certificate(), NOW), null);
    assert.strictEqual(
      certificateValidity(
        certificate({ notBefore: '0001-01-01T00:00:00Z', notAfter: '' }),
        NOW
      ),
      null
    );

    const fresh = certificateValidity(certificate(issued(-30, 60, 30)), NOW);
    assert.strictEqual(Math.round(fresh.percent), 33);
    assert.strictEqual(Math.round(fresh.renewPercent), 67);
    assert.false(fresh.expired);
    assert.false(fresh.renewalDue);
    assert.strictEqual(fresh.tone, 'success');
    assert.strictEqual(fresh.label, 'expires in 2 months');

    const due = certificateValidity(certificate(issued(-80, 10, -20)), NOW);
    assert.true(due.renewalDue);
    assert.strictEqual(due.tone, 'warning');
    assert.strictEqual(due.label, 'expires in 10 days');

    const expired = certificateValidity(certificate(issued(-91, -1, -31)), NOW);
    assert.true(expired.expired);
    assert.false(expired.renewalDue);
    assert.strictEqual(expired.percent, 100);
    assert.strictEqual(expired.tone, 'danger');
    assert.strictEqual(expired.label, 'expired a day ago');
  });

  test('the state of a certificate in one word', function (assert) {
    const state = (o) => certificateState(o, NOW).label;
    const ok = issued(-30, 60, 30);
    assert.strictEqual(state(certificate()), 'not issued');
    assert.strictEqual(
      state(certificate({}, [['Issuing', 'True']])),
      'issuing'
    );
    assert.strictEqual(state(certificate(ok, [['Ready', 'True']])), 'ready');
    assert.strictEqual(
      state(
        certificate(ok, [
          ['Ready', 'True'],
          ['Renewing', 'True'],
        ])
      ),
      'renewing'
    );
    assert.strictEqual(
      state(
        certificate(ok, [
          ['Ready', 'True'],
          ['Renewing', 'True'],
          ['Failed', 'True'],
        ])
      ),
      'failed'
    );
    assert.strictEqual(
      state(certificate(ok, [['IssuerUnavailable', 'True']])),
      'issuer unavailable'
    );
    assert.strictEqual(
      state(certificate({ ...ok, revoke: '0b22' }, [['Ready', 'True']])),
      'revoking'
    );
    assert.strictEqual(
      state(certificate(issued(-80, 10, -20), [['Ready', 'True']])),
      'renewal due'
    );
    assert.strictEqual(
      state(certificate(issued(-91, -1, -31), [['Ready', 'True']])),
      'expired'
    );
    assert.strictEqual(
      state(certificate(ok, [['Ready', 'True']], { DeletionTime: 1 })),
      'deleting'
    );
  });

  test('the names of a certificate and the certificate of an order', function (assert) {
    assert.deepEqual(
      certificateNames({
        Spec: { dns_names: ['a.example.com'], ip_addresses: ['10.0.0.8'] },
      }),
      ['a.example.com', '10.0.0.8']
    );
    assert.deepEqual(certificateNames({}), []);
    assert.deepEqual(
      orderCertificate({ Spec: { certificate: 'staging/api-tls' } }),
      { namespace: 'staging', name: 'api-tls' }
    );
  });

  test('an issuing CA as a row', function (assert) {
    const active = authorityRow(
      {
        ID: 'a1b2c3d4-0000-4000-8000-000000000001',
        Active: true,
        Imported: false,
        NotBefore: iso(NOW - 30 * DAY),
        NotAfter: iso(NOW + 700 * DAY),
        RotatedOutAt: '0001-01-01T00:00:00Z',
        Revoked: [{ Serial: '0a11' }],
      },
      NOW
    );
    assert.strictEqual(active.shortId, 'a1b2c3d4');
    assert.strictEqual(active.state, 'active');
    assert.strictEqual(active.origin, 'made by the servers');
    assert.strictEqual(active.rotatedOut, '', 'the zero time is not a time');
    assert.strictEqual(active.revoked, 1);
    assert.strictEqual(active.expires, 'in 2 years');

    const retired = authorityRow(
      {
        ID: 'e5f6a7b8',
        Active: false,
        Imported: true,
        NotAfter: iso(NOW + 300 * DAY),
        RotatedOutAt: iso(NOW - 30 * DAY),
        Revoked: null,
      },
      NOW
    );
    assert.strictEqual(retired.state, 'retired');
    assert.strictEqual(retired.origin, 'imported');
    assert.strictEqual(retired.rotatedOut, 'a month ago');
    assert.strictEqual(retired.revoked, 0);

    assert.strictEqual(
      authorityRow({ ID: 'x', NotAfter: iso(NOW - DAY) }, NOW).state,
      'expired'
    );
  });
});
