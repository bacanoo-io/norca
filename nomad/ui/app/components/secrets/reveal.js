/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { readSecret } from 'nomad-ui/utils/secrets';

/**
 * The value of a secret version, revealed only on an explicit click by a
 * token with read-secret. The read goes through the value API, which the
 * leader audits. The items live in this component only: not in the URL,
 * the Ember Data store, events or logs. They are shown only for the
 * secret and version they were read for, dropped on Hide, and when the
 * component goes away (navigation).
 *
 *   <Secrets::Reveal @secret={{this.secret}} @version={{3}}
 *     @region={{this.region}} @canRead={{this.canRead}} />
 */
export default class SecretsReveal extends Component {
  @service token;

  @tracked revealed = null; // { key, items }
  @tracked error = null;
  @tracked isLoading = false;

  get key() {
    const s = this.args.secret;
    return `${s?.Namespace}|${s?.Path}|${this.args.version || 0}`;
  }

  get items() {
    return this.revealed?.key === this.key ? this.revealed.items : null;
  }

  get rows() {
    const items = this.items || {};
    return Object.keys(items)
      .sort()
      .map((key) => ({ key, value: items[key] }));
  }

  get versionLabel() {
    return this.args.version
      ? `version ${this.args.version}`
      : 'the current version';
  }

  @action
  async reveal() {
    const s = this.args.secret;
    const key = this.key;
    this.isLoading = true;
    this.error = null;
    this.revealed = null;
    try {
      const { body } = await readSecret(
        this.token,
        this.args.region,
        s.Namespace,
        s.Path,
        this.args.version
      );
      if (this.isDestroying || this.isDestroyed) return;
      this.revealed = { key, items: body?.Items || {} };
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot read the value of this secret (read-secret). The denied read is in the audit trail.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action
  hide() {
    this.revealed = null;
    this.error = null;
  }

  willDestroy() {
    super.willDestroy(...arguments);
    this.revealed = null;
  }
}
