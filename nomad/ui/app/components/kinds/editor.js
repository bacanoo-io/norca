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
  KIND_SCHEMA,
  kindDocument,
  kindBody,
  planKind,
  saveKind,
} from 'nomad-ui/utils/objects';

const PERMISSION = 'write object kinds (operator write)';

/**
 * The editor of a type definition (object kind) in the unified editor: the
 * kind as JSON with its schema as a JSON object, checked locally, then
 * planned on the server: the dry run reports what the change does to the
 * stored objects (how many are checked, converted to a new schema version,
 * changed, or would become invalid) before the write.
 *
 *   <Kinds::Editor @kind={{this.kind}} @region={{this.region}}
 *     @onSaved={{this.saved}} @onClose={{this.close}} />
 */
export default class KindsEditor extends Component {
  @service token;
  @service notifications;

  @tracked text = prettyJSON(kindDocument(this.args.kind));

  schema = KIND_SCHEMA;

  get isNew() {
    return !this.args.kind;
  }

  get title() {
    return this.isNew
      ? 'New type definition'
      : `Edit ${this.args.kind.Group}/${this.args.kind.Kind}`;
  }

  get original() {
    return this.isNew ? '' : prettyJSON(kindDocument(this.args.kind));
  }

  @action
  setText(text) {
    this.text = text;
  }

  @action
  validate(text) {
    const doc = JSON.parse(text);
    const k = this.args.kind;
    if (k && (doc.Group !== k.Group || doc.Kind !== k.Kind))
      return [
        {
          message: `Group and Kind identify the type; keep ${k.Group}/${k.Kind} or create a new type definition.`,
        },
      ];
    if (k && (doc.Version || 1) < (k.Version || 1))
      return [
        {
          message: `Version: the schema version cannot decrease below ${k.Version}.`,
        },
      ];
    return [];
  }

  @action
  async review(text) {
    const doc = JSON.parse(text);
    let plan;
    try {
      plan = (
        await planKind(
          this.token,
          this.args.region,
          doc.Group,
          doc.Kind,
          kindBody(doc)
        )
      ).body;
    } catch (e) {
      throw editorError(e, PERMISSION);
    }
    const m = plan?.Migration || {};
    return {
      migration: m,
      failures: m.Failures || [],
      samples: (m.Samples || []).map((s) => ({
        ...s,
        before: prettyJSON(s.SpecBefore),
        after: prettyJSON(s.SpecAfter),
      })),
      warnings: plan?.Warnings || [],
      original: this.original,
      proposed: text,
    };
  }

  @action
  async submit(text) {
    const doc = JSON.parse(text);
    try {
      await saveKind(
        this.token,
        this.args.region,
        doc.Group,
        doc.Kind,
        kindBody(doc)
      );
    } catch (e) {
      throw editorError(e, PERMISSION);
    }
    this.notifications.add({
      title: `Type definition ${doc.Group}/${doc.Kind} saved`,
      color: 'success',
    });
    this.args.onSaved?.(doc);
  }
}
