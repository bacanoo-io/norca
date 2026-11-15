/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { listObjects, objectHealth } from 'nomad-ui/utils/objects';
import {
  CERTIFICATE_GROUP,
  CERTIFICATE_KIND,
  CERTIFICATE_ISSUER_KIND,
  CERTIFICATE_ORDER_KIND,
  authorityRow,
  certificateNames,
  certificateRevocationList,
  certificateRoots,
  certificateState,
  certificateValidity,
  orderCertificate,
} from 'nomad-ui/utils/certificates';

const KIND = (kind) => `${CERTIFICATE_GROUP}/${kind}`;

// The objects of a kind in every namespace the token can read; an empty
// list where the kind does not exist or the token cannot read it.
function objectsOf(token, region, kind) {
  return listObjects(token, region, CERTIFICATE_GROUP, kind, '*')
    .then((r) => r.body || [])
    .catch((e) => {
      if (e.status === 403 || e.status === 404) return [];
      throw e;
    });
}

/**
 * The certificates the servers keep issued in the region: the Certificate
 * objects with their expiry, the issuers, the open orders with external
 * issuers, and the issuing CAs of the built-in issuer with the trust bundle
 * and the revocation lists. Live from the CustomObject topics of the three
 * kinds.
 */
export default class CertificatesPage extends Component {
  @service token;
  @service system;
  @service eventStream;

  @tracked certificates = [];
  @tracked issuers = [];
  @tracked orders = [];
  @tracked roots = null;
  @tracked rootsError = null;
  @tracked isLoading = true;
  @tracked error = null;

  // the revocation list shown: { authority, shortId, pem } or { error }
  @tracked crl = null;
  @tracked crlLoading = null;

  certificateGroup = CERTIFICATE_GROUP;
  certificateKind = CERTIFICATE_KIND;
  issuerKind = CERTIFICATE_ISSUER_KIND;
  orderKind = CERTIFICATE_ORDER_KIND;
  certificateKindId = KIND(CERTIFICATE_KIND);
  issuerKindId = KIND(CERTIFICATE_ISSUER_KIND);
  orderKindId = KIND(CERTIFICATE_ORDER_KIND);

  get region() {
    return this.system.activeRegion || '';
  }

  get scopeLabel() {
    return this.region
      ? `Kept issued by the servers of the region ${this.region}`
      : 'Kept issued by the servers';
  }

  get certificateRows() {
    return this.certificates
      .map((o) => {
        const validity = certificateValidity(o);
        return {
          name: o.Name,
          namespace: o.Namespace || 'default',
          names: certificateNames(o),
          issuer: o.Spec?.issuer || '',
          secret: o.Spec?.secret_path || '',
          state: certificateState(o),
          validity,
          expires: validity ? validity.label : 'not issued',
          expiresAt: validity ? validity.notAfter.getTime() : Infinity,
        };
      })
      .sort(
        (a, b) =>
          a.expiresAt - b.expiresAt ||
          a.namespace.localeCompare(b.namespace) ||
          a.name.localeCompare(b.name)
      );
  }

  get issuerRows() {
    return this.issuers
      .map((o) => {
        const external = o.Spec?.external;
        const heartbeat = o.Status?.heartbeat;
        return {
          name: o.Name,
          type: external ? 'external' : 'built in',
          controller: external?.controller || '',
          heartbeat: heartbeat ? moment(heartbeat).fromNow() : '',
          health: objectHealth(o),
          used: this.certificates.filter((c) => c.Spec?.issuer === o.Name)
            .length,
        };
      })
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  get orderRows() {
    return this.orders
      .map((o) => {
        const state = o.Status?.state || 'pending';
        return {
          name: o.Name,
          issuer: o.Spec?.issuer || '',
          certificate: orderCertificate(o),
          state,
          tone:
            state === 'failed'
              ? 'danger'
              : state === 'valid'
              ? 'success'
              : 'info',
          problem: o.Status?.problem || '',
          revoke: Boolean(o.Spec?.revoke),
        };
      })
      .sort((a, b) => a.name.localeCompare(b.name));
  }

  get authorityRows() {
    return (this.roots?.Authorities || []).map((ca) => authorityRow(ca));
  }

  get totals() {
    const rows = this.certificateRows;
    const count = (f) => rows.filter(f).length;
    return {
      total: rows.length,
      problems: count((r) => r.state.tone === 'danger'),
      waiting: count(
        (r) => r.state.tone === 'warning' || r.state.tone === 'info'
      ),
    };
  }

  get bundle() {
    return this.roots?.Roots || '';
  }

  // The trust bundle as a link the browser saves as a file.
  get bundleHref() {
    return `data:application/x-pem-file;charset=utf-8,${encodeURIComponent(
      this.bundle
    )}`;
  }

  get bundleFile() {
    return `norca-certificate-roots${this.region ? `-${this.region}` : ''}.pem`;
  }

  async load() {
    try {
      const [certificates, issuers, orders] = await Promise.all([
        objectsOf(this.token, this.region, CERTIFICATE_KIND),
        objectsOf(this.token, this.region, CERTIFICATE_ISSUER_KIND),
        objectsOf(this.token, this.region, CERTIFICATE_ORDER_KIND),
      ]);
      if (this.isDestroying || this.isDestroyed) return;
      this.certificates = certificates;
      this.issuers = issuers;
      this.orders = orders;
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  async loadRoots() {
    try {
      const { body } = await certificateRoots(this.token, this.region);
      if (this.isDestroying || this.isDestroyed) return;
      this.roots = body;
      this.rootsError = null;
    } catch (e) {
      this.roots = null;
      this.rootsError =
        e.status === 403
          ? 'The servers did not accept the token: sign in again to see the issuing CAs.'
          : errorMessage(e);
    }
  }

  @action
  async showRevocationList(row) {
    this.crlLoading = row.id;
    try {
      const { body } = await certificateRevocationList(
        this.token,
        this.region,
        row.id
      );
      this.crl = { authority: row.id, shortId: row.shortId, pem: body?.CRL };
    } catch (e) {
      this.crl = {
        authority: row.id,
        shortId: row.shortId,
        error: errorMessage(e),
      };
    } finally {
      this.crlLoading = null;
    }
  }

  @action
  closeRevocationList() {
    this.crl = null;
  }

  setup = modifier((element, [region]) => {
    waitForPromise(
      Promise.resolve().then(() => Promise.all([this.load(), this.loadRoots()]))
    );
    const unsubscribe = this.eventStream.subscribe({
      topics: [
        `CustomObject:${KIND(CERTIFICATE_KIND)}`,
        `CustomObject:${KIND(CERTIFICATE_ISSUER_KIND)}`,
        `CustomObject:${KIND(CERTIFICATE_ORDER_KIND)}`,
      ],
      region,
      namespace: '*',
      // A revocation and a rotation change the CAs without an event of
      // their own: the trust bundle is read again with the objects.
      onEvents: () =>
        waitForPromise(Promise.all([this.load(), this.loadRoots()])),
      onResync: () => Promise.all([this.load(), this.loadRoots()]),
    });
    return () => unsubscribe();
  });
}
