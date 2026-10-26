/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsSecret } from 'nomad-ui/utils/norca-acl';
import {
  readSecret,
  writeSecret,
  validSecretPath,
  validItemKey,
} from 'nomad-ui/utils/secrets';

let rowId = 0;
const newRow = (key = '', value = '') => ({ id: ++rowId, key, value });

/**
 * The form that writes a secret value: a new secret (namespace, path, items
 * and a description and owner) or a new version of @secret (its items). A
 * write is conditional on the check index: 0 for a new secret, the
 * secret's modify index for a new version, so a concurrent change is not
 * overwritten. Values are typed into password fields and live only in this
 * component; they are dropped when it closes.
 *
 *   <Secrets::ValueForm @secret={{this.secret}} @region={{this.region}}
 *     @onSaved={{this.saved}} @onClose={{this.close}} />
 */
export default class SecretsValueForm extends Component {
  @service token;
  @service notifications;

  @tracked namespace =
    this.args.secret?.Namespace || this.args.namespace || 'default';
  @tracked path = '';
  @tracked description = '';
  @tracked owner = '';
  @tracked rows = (this.args.secret?.ItemKeys || ['']).map((k) => newRow(k));
  @tracked showValues = false;
  @tracked error = null;
  @tracked isSaving = false;
  @tracked isFilling = false;
  @tracked submitted = false;

  get isNew() {
    return !this.args.secret;
  }

  get title() {
    return this.isNew
      ? 'New secret'
      : `New version of ${this.args.secret.Path}`;
  }

  get targetPath() {
    return this.isNew ? this.path.trim() : this.args.secret.Path;
  }

  get canFill() {
    return (
      !this.isNew &&
      !this.args.secret.Deleted &&
      !this.args.secret.Destroyed &&
      allowsSecret(
        this.token,
        this.args.secret.Namespace,
        this.args.secret.Path,
        'read-secret'
      )
    );
  }

  get pathError() {
    if (!this.isNew || !this.submitted) return null;
    return validSecretPath(this.targetPath)
      ? null
      : 'Use letters, digits, -, _ and ~ in segments separated by /, without a leading or trailing /.';
  }

  get problems() {
    const problems = [];
    if (this.isNew && !validSecretPath(this.targetPath))
      problems.push('The path is not valid.');
    if (!this.rows.length) problems.push('A secret needs at least one item.');
    const keys = this.rows.map((r) => r.key.trim());
    if (keys.some((k) => !validItemKey(k)))
      problems.push(
        'Item keys use letters, digits, _, . and - (at most 128 characters).'
      );
    if (new Set(keys).size !== keys.length)
      problems.push('Item keys must be unique.');
    if (this.rows.some((r) => r.value === ''))
      problems.push('Every item needs a value.');
    return problems;
  }

  get nextVersion() {
    return (this.args.secret?.CurrentVersion || 0) + 1;
  }

  get itemRows() {
    return this.rows.map((r, i) => ({ ...r, n: i + 1 }));
  }

  get valueType() {
    return this.showValues ? 'text' : 'password';
  }

  @action setNamespace(value) {
    this.namespace = value;
  }

  @action setPath(value) {
    this.path = value;
  }

  @action setDescription(value) {
    this.description = value;
  }

  @action setOwner(value) {
    this.owner = value;
  }

  @action setKey(row, value) {
    this.rows = this.rows.map((r) =>
      r.id === row.id ? { ...r, key: value } : r
    );
  }

  @action setValue(row, value) {
    this.rows = this.rows.map((r) => (r.id === row.id ? { ...r, value } : r));
  }

  @action addRow() {
    this.rows = [...this.rows, newRow()];
  }

  @action removeRow(row) {
    this.rows = this.rows.filter((r) => r.id !== row.id);
  }

  @action toggleValues() {
    this.showValues = !this.showValues;
  }

  // Fills in the values of the current version (an explicit, audited
  // read), so that a new version can change some items and keep others.
  @action
  async fillCurrent() {
    const s = this.args.secret;
    this.isFilling = true;
    this.error = null;
    try {
      const { body } = await readSecret(
        this.token,
        this.args.region,
        s.Namespace,
        s.Path
      );
      const items = body?.Items || {};
      this.rows = Object.keys(items)
        .sort()
        .map((k) => newRow(k, items[k]));
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot read the value of this secret (read-secret).'
          : errorMessage(e);
    } finally {
      this.isFilling = false;
    }
  }

  @action
  async submit(event) {
    event?.preventDefault();
    this.submitted = true;
    if (this.problems.length) return;
    this.isSaving = true;
    this.error = null;
    const items = Object.fromEntries(
      this.rows.map((r) => [r.key.trim(), r.value])
    );
    const meta = this.isNew
      ? {
          Description: this.description.trim(),
          Owner: this.owner.trim(),
        }
      : undefined;
    try {
      const { body } = await writeSecret(
        this.token,
        this.args.region,
        this.namespace,
        this.targetPath,
        {
          items,
          meta,
          cas: this.isNew ? 0 : this.args.secret.ModifyIndex,
        }
      );
      const saved = body?.Secret || {
        Namespace: this.namespace,
        Path: this.targetPath,
      };
      this.rows = [];
      this.notifications.add({
        title: this.isNew
          ? `Secret ${saved.Path} created`
          : `Version ${saved.CurrentVersion} of ${saved.Path} written`,
        message:
          'Tasks that use the latest version receive it per their change mode.',
        color: 'success',
      });
      this.args.onSaved?.(saved);
    } catch (e) {
      if (e.status === 409)
        this.error = this.isNew
          ? 'A secret with this path already exists in the namespace. Open it and write a new version instead.'
          : 'The secret changed since the page loaded it. Close this form, check the secret and write the version again.';
      else if (e.status === 403)
        this.error = `Your token cannot write secrets at this path in namespace ${this.namespace} (write-secret).`;
      else this.error = errorMessage(e);
    } finally {
      this.isSaving = false;
    }
  }

  @action
  close() {
    this.rows = [];
    this.args.onClose?.();
  }

  willDestroy() {
    super.willDestroy(...arguments);
    this.rows = [];
  }
}
