/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import moment from 'moment';
import { apiRequest } from 'nomad-ui/utils/norca-api';
import { conditions } from 'nomad-ui/utils/objects';

// Certificates the servers keep issued: the kinds norca.io/CertificateIssuer,
// Certificate and CertificateOrder, the issuing CAs of the region with their
// trust bundle and revocation lists, and the revocation of a certificate.

const enc = encodeURIComponent;

export const CERTIFICATE_GROUP = 'norca.io';
export const CERTIFICATE_KIND = 'Certificate';
export const CERTIFICATE_ISSUER_KIND = 'CertificateIssuer';
export const CERTIFICATE_ORDER_KIND = 'CertificateOrder';
export const CERTIFICATE_KIND_ID = `${CERTIFICATE_GROUP}/${CERTIFICATE_KIND}`;

// The label of a secret written for a Certificate object: the object name.
export const SECRET_LABEL_CERTIFICATE = 'norca.io/certificate';

// The issuing CAs of the region (the active one first), and the PEM bundle
// a client of the certificates has to trust.
export function certificateRoots(token, region) {
  return apiRequest(token, '/v1/certificates/roots', { params: { region } });
}

// The PEM revocation list of an issuing CA: of the active one without
// authority.
export function certificateRevocationList(token, region, authority) {
  return apiRequest(token, '/v1/certificates/crl', {
    params: { region, authority },
  });
}

// Revokes the current certificate of a Certificate object; the servers issue
// a new one.
export function revokeCertificate(token, region, namespace, name) {
  return apiRequest(token, `/v1/certificate/${enc(name)}/revoke`, {
    method: 'PUT',
    params: { region, namespace },
  });
}

function time(value) {
  if (!value) return null;
  const m = moment(value);
  // The zero time of Go ("0001-01-01T00:00:00Z") means "not set".
  return m.isValid() && m.year() > 1 ? m : null;
}

function conditionIsTrue(object, type) {
  return conditions(object).some(
    (c) =>
      (c.type || c.Type) === type && String(c.status ?? c.Status) === 'True'
  );
}

// The validity of a certificate from the status of its object, against now:
// how much of it has passed, where the renewal is on it, and what to say
// about the expiry. Null while no certificate is issued.
export function certificateValidity(object, now = Date.now()) {
  const status = object?.Status || {};
  const notBefore = time(status.notBefore);
  const notAfter = time(status.notAfter);
  if (!notBefore || !notAfter) return null;
  const renewAt = time(status.renewAt);
  const total = notAfter.valueOf() - notBefore.valueOf();
  const share = (t) =>
    total > 0
      ? Math.min(Math.max(((t - notBefore.valueOf()) / total) * 100, 0), 100)
      : 100;
  const expired = now >= notAfter.valueOf();
  const renewalDue = !expired && renewAt !== null && now >= renewAt.valueOf();
  return {
    notBefore: notBefore.toDate(),
    notAfter: notAfter.toDate(),
    renewAt: renewAt ? renewAt.toDate() : null,
    percent: share(now),
    renewPercent: renewAt ? share(renewAt.valueOf()) : null,
    expired,
    renewalDue,
    tone: expired ? 'danger' : renewalDue ? 'warning' : 'success',
    color: expired ? 'red' : renewalDue ? 'orange' : 'green',
    label: expired
      ? `expired ${notAfter.from(now)}`
      : `expires ${notAfter.from(now)}`,
  };
}

// What the certificate of an object is doing, in one word, with a tone:
// the Ready, Failed, IssuerUnavailable, Issuing and Renewing conditions and
// the expiry.
export function certificateState(object, now = Date.now()) {
  const validity = certificateValidity(object, now);
  if (object?.DeletionTime) return { label: 'deleting', tone: 'warning' };
  if (validity?.expired) return { label: 'expired', tone: 'danger' };
  if (conditionIsTrue(object, 'Failed'))
    return { label: 'failed', tone: 'danger' };
  if (conditionIsTrue(object, 'IssuerUnavailable'))
    return { label: 'issuer unavailable', tone: 'warning' };
  if (object?.Status?.revoke) return { label: 'revoking', tone: 'info' };
  if (conditionIsTrue(object, 'Renewing'))
    return { label: 'renewing', tone: 'info' };
  if (conditionIsTrue(object, 'Issuing'))
    return { label: 'issuing', tone: 'info' };
  if (validity?.renewalDue) return { label: 'renewal due', tone: 'warning' };
  if (conditionIsTrue(object, 'Ready'))
    return { label: 'ready', tone: 'success' };
  return { label: validity ? 'issued' : 'not issued', tone: 'neutral' };
}

// The names a certificate is for: DNS names, then IP addresses.
export function certificateNames(object) {
  const spec = object?.Spec || {};
  return [...(spec.dns_names || []), ...(spec.ip_addresses || [])];
}

// The certificate "<namespace>/<name>" an order was placed for.
export function orderCertificate(order) {
  const ref = String(order?.Spec?.certificate || '');
  const at = ref.indexOf('/');
  return at < 0
    ? { namespace: 'default', name: ref }
    : { namespace: ref.slice(0, at), name: ref.slice(at + 1) };
}

// An issuing CA as a table row: active, retired (kept in the trust bundle
// until what it signed has expired) or expired.
export function authorityRow(ca, now = Date.now()) {
  const notAfter = time(ca.NotAfter);
  const rotatedOut = time(ca.RotatedOutAt);
  const expired = notAfter !== null && now >= notAfter.valueOf();
  return {
    id: ca.ID,
    shortId: String(ca.ID || '').slice(0, 8),
    active: Boolean(ca.Active),
    origin: ca.Imported ? 'imported' : 'made by the servers',
    notBefore: time(ca.NotBefore)?.toDate() ?? null,
    notAfter: notAfter?.toDate() ?? null,
    expires: notAfter ? notAfter.from(now) : '–',
    rotatedOut: rotatedOut ? rotatedOut.from(now) : '',
    revoked: (ca.Revoked || []).length,
    state: expired ? 'expired' : ca.Active ? 'active' : 'retired',
    tone: expired ? 'danger' : ca.Active ? 'success' : 'neutral',
  };
}
