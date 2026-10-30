/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { editorError, prettyJSON } from 'nomad-ui/utils/norca-editor';
import {
  writeSecret,
  secretMeta,
  SECRET_META_SCHEMA,
} from 'nomad-ui/utils/secrets';

/**
 * The metadata of a secret (description, owner, labels, expiry, rotation
 * interval, kept versions) in the unified editor. Metadata is never
 * secret; the write replaces it without writing a version and is
 * conditional on the loaded secret.
 */
export default class SecretsMetaEditor extends Component {
  @service token;
  @service notifications;

  schema = SECRET_META_SCHEMA;

  @tracked text = prettyJSON(secretMeta(this.args.secret));

  get original() {
    return prettyJSON(secretMeta(this.args.secret));
  }

  @action
  setText(text) {
    this.text = text;
  }

  @action
  async submit(text) {
    const meta = JSON.parse(text);
    const s = this.args.secret;
    try {
      await writeSecret(this.token, this.args.region, s.Namespace, s.Path, {
        meta: {
          ...meta,
          Labels: meta.Labels || null,
          ExpiresAt: meta.ExpiresAt || null,
          RotateEvery: meta.RotateEvery || 0,
          MaxVersions: meta.MaxVersions || 0,
        },
        cas: s.ModifyIndex,
      });
    } catch (e) {
      if (e.status === 409) {
        const err = new Error(
          'The secret changed since the page loaded it. Close the editor and edit again.'
        );
        err.errors = [{ status: '409', detail: err.message }];
        throw err;
      }
      throw editorError(e, 'write secrets at this path (write-secret)');
    }
    this.notifications.add({
      title: `Metadata of ${s.Path} saved`,
      color: 'success',
    });
    this.args.onSaved?.();
  }
}
