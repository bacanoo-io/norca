/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { collection, plain, error, json } from './util';
import { secretRecord } from './secrets';

// Mirage routes of the certificates the servers keep issued: the issuing
// CAs of the region (norcaCertificateAuthorities: CertificateAuthority
// JSON), their trust bundle and revocation lists, and the revocation of a
// certificate. The objects themselves are custom objects (norcaObjects) of
// the kinds norca.io/CertificateIssuer, Certificate and CertificateOrder.

const GROUP = 'norca.io';
const DAY = 86400000;

function pem(label, text) {
  return `-----BEGIN ${label}-----\n${btoa(text)}\n-----END ${label}-----\n`;
}

function authorities(db) {
  return collection(db, 'norcaCertificateAuthorities')
    .map(plain)
    .sort((a, b) => Number(b.Active) - Number(a.Active));
}

export default function registerCertificateRoutes(server) {
  server.get('/certificates/roots', function ({ db }) {
    const list = authorities(db);
    return json({
      ActiveID: list.find((ca) => ca.Active)?.ID || '',
      Authorities: list,
      Roots: list.map((ca) => ca.Cert).join(''),
    });
  });

  server.get('/certificates/crl', function ({ db }, { queryParams }) {
    const list = authorities(db);
    const ca = queryParams.authority
      ? list.find((a) => a.ID === queryParams.authority)
      : list.find((a) => a.Active);
    if (!ca) return error(404, 'issuing CA not found');
    return json({
      CRL: pem(
        'X509 CRL',
        `crl of ${ca.ID}: ${(ca.Revoked || []).map((r) => r.Serial).join(',')}`
      ),
    });
  });

  // The revocation: the serial goes on the list of its CA and the object
  // says which serial is being replaced, as the operator records it.
  server.put('/certificate/:name/revoke', function ({ db }, request) {
    const namespace = request.queryParams.namespace || 'default';
    const objects = collection(db, 'norcaObjects');
    const o = objects.where(
      (x) =>
        x.Group === GROUP &&
        x.Kind === 'Certificate' &&
        x.Name === request.params.name &&
        (x.Namespace || 'default') === namespace
    )[0];
    if (!o) return error(404, 'certificate not found');
    const serial = o.Status?.serial;
    if (!serial) return error(400, 'the certificate has not been issued yet');
    objects.update(o.id, {
      Status: { ...o.Status, revoke: serial },
      ModifyIndex: (o.ModifyIndex || 0) + 1,
    });
    const cas = collection(db, 'norcaCertificateAuthorities');
    const ca = cas.findBy({ ID: o.Status.authority });
    if (ca)
      cas.update(ca.id, {
        Revoked: [
          ...(ca.Revoked || []),
          {
            Serial: serial,
            Certificate: `${namespace}/${o.Name}`,
            RevokedAt: new Date().toISOString(),
            NotAfter: o.Status.notAfter,
          },
        ],
      });
    return json({ Serial: serial }, (o.ModifyIndex || 0) + 1);
  });
}

function condition(type, status, reason, message = '') {
  return {
    type,
    status,
    reason,
    message,
    lastTransitionTime: new Date().toISOString(),
  };
}

const iso = (ms) => new Date(ms).toISOString();

/**
 * Seeds the three kinds of certificates, the built-in issuer and an
 * external one, four certificates (ready, renewal due, failed, waiting for
 * an external issuer with an open order), two issuing CAs (the active one
 * and a retired one) and the secret of the ready certificate.
 */
export function seedCertificates(server) {
  const db = server.db;
  const now = Date.now();
  const kinds = collection(db, 'norcaObjectKinds');
  const kind = (attrs) =>
    kinds.insert({
      Group: GROUP,
      Description: '',
      Schema: '',
      Rules: null,
      Scale: null,
      Replicated: false,
      Version: 1,
      Migration: null,
      Webhooks: null,
      Limits: null,
      CreateIndex: 3,
      ModifyIndex: 3,
      ...attrs,
    });
  kind({
    Kind: 'CertificateIssuer',
    Description: 'Who signs certificates (built in)',
    Namespaced: false,
    Columns: [
      { Name: 'Controller', Path: 'spec.external.controller' },
      { Name: 'Heartbeat', Path: 'status.heartbeat' },
      { Name: 'Authority', Path: 'status.authority' },
    ],
  });
  kind({
    Kind: 'Certificate',
    Description:
      'TLS certificate for DNS names, kept issued and renewed in the secret store (built in)',
    Namespaced: true,
    Columns: [
      { Name: 'Issuer', Path: 'spec.issuer' },
      { Name: 'Secret', Path: 'spec.secret_path' },
      { Name: 'Not After', Path: 'status.notAfter' },
      { Name: 'Renew At', Path: 'status.renewAt' },
      { Name: 'Serial', Path: 'status.serial' },
    ],
  });
  kind({
    Kind: 'CertificateOrder',
    Description: 'One issuance handed to an external issuer (built in)',
    Namespaced: false,
    Columns: [
      { Name: 'Issuer', Path: 'spec.issuer' },
      { Name: 'Certificate', Path: 'spec.certificate' },
      { Name: 'State', Path: 'status.state' },
      { Name: 'Problem', Path: 'status.problem' },
    ],
  });

  const active = 'a1b2c3d4-0000-4000-8000-000000000001';
  const retired = 'e5f6a7b8-0000-4000-8000-000000000002';
  const cas = collection(db, 'norcaCertificateAuthorities');
  cas.insert({
    ID: active,
    Region: 'global',
    Cert: pem('CERTIFICATE', 'issuing CA 1 of the region'),
    Chain: '',
    Active: true,
    Imported: false,
    NotBefore: iso(now - 30 * DAY),
    NotAfter: iso(now + 3620 * DAY),
    RotatedOutAt: '0001-01-01T00:00:00Z',
    Revoked: [
      {
        Serial: '0a11',
        Certificate: 'default/web-tls',
        RevokedAt: iso(now - 2 * DAY),
        NotAfter: iso(now + 40 * DAY),
      },
    ],
    CreateIndex: 40,
    ModifyIndex: 44,
  });
  cas.insert({
    ID: retired,
    Region: 'global',
    Cert: pem('CERTIFICATE', 'issuing CA 0 of the region'),
    Chain: '',
    Active: false,
    Imported: true,
    NotBefore: iso(now - 400 * DAY),
    NotAfter: iso(now + 300 * DAY),
    RotatedOutAt: iso(now - 30 * DAY),
    Revoked: [],
    CreateIndex: 12,
    ModifyIndex: 40,
  });

  const objects = collection(db, 'norcaObjects');
  const base = {
    Group: GROUP,
    Labels: null,
    ManagedBy: '',
    Owners: null,
    Finalizers: null,
    DeletionTime: 0,
    KindVersion: 1,
    Generation: 1,
    CreateTime: (now - 20 * DAY) * 1e6,
    ModifyTime: now * 1e6,
    CreateIndex: 50,
    ModifyIndex: 60,
    Events: [],
  };
  objects.insert({
    ...base,
    Kind: 'CertificateIssuer',
    Namespace: '',
    Name: 'internal',
    Spec: { builtin: {} },
    Status: {
      observedGeneration: 1,
      authority: active,
      conditions: [condition('Ready', 'True', 'AuthorityActive')],
    },
  });
  objects.insert({
    ...base,
    Kind: 'CertificateIssuer',
    Namespace: '',
    Name: 'acme',
    Spec: { external: { controller: 'norca-acme' } },
    Status: {
      observedGeneration: 1,
      heartbeat: iso(now - 20000),
      conditions: [condition('Ready', 'True', 'ControllerAlive')],
    },
  });

  const certificate = (attrs) =>
    objects.insert({
      ...base,
      Kind: 'Certificate',
      Namespace: 'default',
      Finalizers: ['norca.io/operators'],
      ...attrs,
    });
  certificate({
    Name: 'web-tls',
    Spec: {
      issuer: 'internal',
      dns_names: ['app.example.com', '*.apps.example.com'],
      secret_path: 'certs/web',
    },
    Status: {
      observedGeneration: 1,
      issuer: 'internal',
      authority: active,
      serial: '0b22',
      notBefore: iso(now - 10 * DAY),
      notAfter: iso(now + 80 * DAY),
      renewAt: iso(now + 50 * DAY),
      secretVersion: 2,
      conditions: [
        condition('Ready', 'True', 'Issued'),
        condition('Issuing', 'False', 'Issued'),
        condition('Failed', 'False', 'Issued'),
      ],
    },
    Events: [
      {
        Type: 'Normal',
        Reason: 'Issued',
        Message: 'serial 0b22, valid for 90 days',
        Reporter: 'norca-operators',
        Count: 1,
        FirstTime: (now - 10 * DAY) * 1e6,
        LastTime: (now - 10 * DAY) * 1e6,
      },
    ],
  });
  certificate({
    Name: 'api-tls',
    Namespace: 'staging',
    Spec: {
      issuer: 'internal',
      dns_names: ['api.staging.example.com'],
      ip_addresses: ['10.0.0.8'],
      secret_path: 'certs/api',
    },
    Status: {
      observedGeneration: 1,
      issuer: 'internal',
      authority: retired,
      serial: '0c33',
      notBefore: iso(now - 85 * DAY),
      notAfter: iso(now + 5 * DAY),
      renewAt: iso(now - 25 * DAY),
      secretVersion: 1,
      failures: 3,
      retryAt: iso(now + 600000),
      conditions: [
        condition('Ready', 'True', 'Issued'),
        condition('Renewing', 'True', 'RenewalDue'),
        condition(
          'Failed',
          'True',
          'SecretWrite',
          'the secret store is sealed'
        ),
      ],
    },
  });
  certificate({
    Name: 'shop-tls',
    Spec: {
      issuer: 'acme',
      dns_names: ['shop.example.com'],
      secret_path: 'certs/shop',
    },
    Status: {
      observedGeneration: 1,
      order: 'default.shop-tls.1',
      orders: 1,
      conditions: [
        condition('Ready', 'False', 'NotIssued'),
        condition('Issuing', 'True', 'OrderPlaced'),
      ],
    },
  });
  objects.insert({
    ...base,
    Kind: 'CertificateOrder',
    Namespace: '',
    Name: 'default.shop-tls.1',
    ManagedBy: 'norca-operators',
    Spec: {
      issuer: 'acme',
      certificate: 'default/shop-tls',
      csr: pem('CERTIFICATE REQUEST', 'shop.example.com'),
      dns_names: ['shop.example.com'],
    },
    Status: { state: 'processing' },
  });

  collection(db, 'norcaSecrets').insert(
    secretRecord({
      path: 'certs/web',
      description: 'Certificate of web-tls',
      labels: { 'norca.io/certificate': 'web-tls' },
      values: [
        { cert: 'first-certificate', key: 'first-key', ca: 'issuing-ca' },
        { cert: 'second-certificate', key: 'second-key', ca: 'issuing-ca' },
      ],
      index: 180,
    })
  );
}
