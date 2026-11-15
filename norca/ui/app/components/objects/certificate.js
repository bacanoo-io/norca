/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsObject, allowsSecret } from 'nomad-ui/utils/norca-acl';
import {
  CERTIFICATE_GROUP,
  CERTIFICATE_ISSUER_KIND,
  CERTIFICATE_KIND_ID,
  CERTIFICATE_ORDER_KIND,
  certificateNames,
  certificateState,
  certificateValidity,
  revokeCertificate,
} from 'nomad-ui/utils/certificates';

function when(value) {
  if (!value) return '';
  const m = moment(value);
  return m.isValid() ? m.fromNow() : String(value);
}

/**
 * The page of a norca.io/Certificate object: the names, the issuer, the
 * secret the certificate is kept in, the serial number and the issuing CA,
 * the validity with the time of the renewal, the open order with an external
 * issuer, what failed and when it is tried again, and the revocation.
 * @object is the object, @region its region, @onChanged is called after a
 * revocation was asked for.
 */
export default class ObjectsCertificate extends Component {
  @service token;
  @service notifications;

  @tracked revoking = false;
  @tracked isRevoking = false;
  @tracked revokeError = null;

  issuerGroup = CERTIFICATE_GROUP;
  issuerKind = CERTIFICATE_ISSUER_KIND;
  orderKind = CERTIFICATE_ORDER_KIND;

  get spec() {
    return this.args.object?.Spec || {};
  }

  get status() {
    return this.args.object?.Status || {};
  }

  get namespace() {
    return this.args.object?.Namespace || 'default';
  }

  get state() {
    return certificateState(this.args.object);
  }

  get validity() {
    return certificateValidity(this.args.object);
  }

  get names() {
    return certificateNames(this.args.object);
  }

  get issuer() {
    return this.spec.issuer || this.status.issuer || '';
  }

  // The issuer changed in the spec and the certificate of the old one is
  // still the current one.
  get issuedBy() {
    const issued = this.status.issuer;
    return issued && issued !== this.spec.issuer ? issued : '';
  }

  get secretPath() {
    return this.spec.secret_path || '';
  }

  get canSeeSecret() {
    return (
      this.secretPath &&
      allowsSecret(this.token, this.namespace, this.secretPath, 'list-secrets')
    );
  }

  get shortAuthority() {
    return String(this.status.authority || '').slice(0, 8);
  }

  get validityText() {
    const v = this.validity;
    if (!v) return '';
    return `${Math.round(v.percent)}% of the validity has passed`;
  }

  get renewal() {
    const v = this.validity;
    if (!v?.renewAt) return '';
    return v.renewalDue || v.expired
      ? `was due ${when(v.renewAt)}`
      : `${when(v.renewAt)}`;
  }

  get retry() {
    const s = this.status;
    if (!s.failures) return null;
    return {
      failures: s.failures,
      at: s.retryAt ? when(s.retryAt) : '',
    };
  }

  get canRevoke() {
    return (
      Boolean(this.status.serial) &&
      !this.status.revoke &&
      !this.args.object?.DeletionTime &&
      allowsObject(this.token, this.namespace, CERTIFICATE_KIND_ID, 'write')
    );
  }

  @action
  openRevoke() {
    this.revokeError = null;
    this.revoking = true;
  }

  @action
  closeRevoke() {
    this.revoking = false;
  }

  @action
  async confirmRevoke() {
    const name = this.args.object.Name;
    this.isRevoking = true;
    this.revokeError = null;
    try {
      const { body } = await revokeCertificate(
        this.token,
        this.args.region,
        this.namespace,
        name
      );
      this.revoking = false;
      this.notifications.add({
        title: `The certificate of ${name} is being revoked`,
        message: `Serial ${
          body?.Serial || this.status.serial
        } goes on the revocation list; a new certificate is issued into the same secret.`,
        color: 'success',
      });
      this.args.onChanged?.();
    } catch (e) {
      this.revokeError =
        e.status === 403
          ? `Your token cannot revoke certificates in the namespace ${
              this.namespace
            } (object write on ${CERTIFICATE_KIND_ID}): ${errorMessage(e)}`
          : errorMessage(e);
    } finally {
      this.isRevoking = false;
    }
  }
}
