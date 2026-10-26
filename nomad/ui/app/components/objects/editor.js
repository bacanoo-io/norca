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
  applyObject,
  kindId,
  objectDocument,
  objectDocumentSchema,
} from 'nomad-ui/utils/objects';

const PERMISSION = 'write objects of this kind (object write)';

/**
 * The editor of a custom object in the unified editor: the owner's part of
 * the object (name, namespace, labels, spec) as JSON, checked against the
 * spec schema of its kind while typing, then a dry run on the server
 * (schema with defaults, CEL rules, admission webhooks) whose result is
 * shown as a diff before the write. The write is conditional on the
 * version that was edited.
 *
 *   <Objects::Editor @kind={{this.kind}} @object={{this.object}}
 *     @region={{this.region}} @onSaved={{this.saved}} @onClose={{this.close}} />
 *
 * Without @object it creates an object of the kind (in @namespace).
 */
export default class ObjectsEditor extends Component {
  @service token;
  @service notifications;

  @tracked text = prettyJSON(
    objectDocument(
      this.args.object || { Namespace: this.args.namespace },
      this.args.kind
    )
  );

  get isNew() {
    return !this.args.object;
  }

  get schema() {
    return objectDocumentSchema(this.args.kind);
  }

  get kindId() {
    return kindId(this.args.kind);
  }

  get title() {
    return this.isNew
      ? `New ${this.args.kind.Kind}`
      : `Edit ${this.args.kind.Kind} ${this.args.object.Name}`;
  }

  get original() {
    return this.isNew
      ? ''
      : prettyJSON(objectDocument(this.args.object, this.args.kind));
  }

  get stored() {
    return this.isNew
      ? ''
      : prettyJSON({
          Labels: this.args.object.Labels || {},
          Spec: this.args.object.Spec || {},
        });
  }

  ref(doc) {
    return {
      group: this.args.kind.Group,
      kind: this.args.kind.Kind,
      name: doc.Name,
      namespace: this.args.kind.Namespaced
        ? doc.Namespace || 'default'
        : undefined,
    };
  }

  body(doc) {
    return {
      Group: this.args.kind.Group,
      Kind: this.args.kind.Kind,
      Name: doc.Name,
      Namespace: this.args.kind.Namespaced ? doc.Namespace || 'default' : '',
      Labels: doc.Labels || {},
      Spec: doc.Spec,
    };
  }

  get cas() {
    return this.isNew ? 0 : this.args.object.ModifyIndex;
  }

  @action
  setText(text) {
    this.text = text;
  }

  @action
  validate(text) {
    const doc = JSON.parse(text);
    const o = this.args.object;
    if (o && doc.Name !== o.Name)
      return [
        {
          message: `Name: an object cannot be renamed; keep "${o.Name}" or create a new object.`,
        },
      ];
    if (o && this.args.kind.Namespaced && doc.Namespace !== o.Namespace)
      return [
        {
          message: `Namespace: an object cannot be moved; keep "${o.Namespace}".`,
        },
      ];
    return [];
  }

  // The dry run: the object as the server would store it.
  @action
  async review(text) {
    const doc = JSON.parse(text);
    let result;
    try {
      result = (
        await applyObject(
          this.token,
          this.args.region,
          this.ref(doc),
          this.body(doc),
          { dryRun: true, cas: this.cas }
        )
      ).body;
    } catch (e) {
      throw editorError(e, PERMISSION);
    }
    return {
      object: result,
      stored: this.stored,
      proposed: prettyJSON({
        Labels: result?.Labels || {},
        Spec: result?.Spec || {},
      }),
      defaulted:
        JSON.stringify(result?.Spec || {}) !== JSON.stringify(doc.Spec || {}),
    };
  }

  @action
  async submit(text) {
    const doc = JSON.parse(text);
    let saved;
    try {
      saved = (
        await applyObject(
          this.token,
          this.args.region,
          this.ref(doc),
          this.body(doc),
          { cas: this.cas }
        )
      ).body;
    } catch (e) {
      throw editorError(e, PERMISSION);
    }
    this.notifications.add({
      title: `${this.args.kind.Kind} ${doc.Name} ${
        this.isNew ? 'created' : 'saved'
      }`,
      message: 'Its controller reconciles the new spec.',
      color: 'success',
    });
    this.args.onSaved?.(saved || this.body(doc));
  }
}
