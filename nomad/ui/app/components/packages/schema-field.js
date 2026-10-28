/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { draftOf, isTextWidget } from 'nomad-ui/utils/pack-form';

/**
 * One field of the pack install form (utils/pack-form), by its widget.
 * Object variables render their attributes as nested fields. Lists, maps
 * and JSON values are edited as text: @drafts keeps the text by field id
 * while it does not parse.
 *
 *   <Packages::SchemaField @field={{f}} @value={{v}} @errors={{this.errors}}
 *     @drafts={{this.drafts}} @namespaces={{ns}} @regions={{regions}}
 *     @onChange={{this.setValue}} @onDraft={{this.setDraft}} />
 */
export default class PackagesSchemaField extends Component {
  @tracked revealed = false;

  get field() {
    return this.args.field;
  }

  get error() {
    return this.args.errors?.[this.field.id];
  }

  get label() {
    const f = this.field;
    return f.unit ? `${f.title} (${f.unit})` : f.title;
  }

  get text() {
    const draft = this.args.drafts?.[this.field.id];
    return draft !== undefined ? draft : draftOf(this.field, this.args.value);
  }

  get isText() {
    return isTextWidget(this.field);
  }

  get textHelp() {
    const hint = {
      tags: 'Comma or line separated.',
      'key-value': 'One key=value per line.',
      json: 'JSON.',
    }[this.field.widget];
    return [this.field.description, hint].filter(Boolean).join(' ');
  }

  get segments() {
    return (this.field.enum || []).map((v) => ({
      value: v,
      label: this.field.enumLabels[v] || String(v),
    }));
  }

  get help() {
    const f = this.field;
    if (this.isText) return this.textHelp;
    if (f.secretPath) {
      const where = `Written to the ${f.secretItem} item of the ${f.secretPath} Variable, never to the release.`;
      const keep = this.args.isUpgrade
        ? ' Leave empty to keep the stored value.'
        : '';
      return [f.description, where + keep].filter(Boolean).join(' ');
    }
    return f.description;
  }

  get value() {
    return this.args.value ?? '';
  }

  get options() {
    const f = this.field;
    let choices = f.enum || [];
    if (f.widget === 'namespace-select') choices = this.args.namespaces || [];
    if (f.widget === 'region-select') choices = this.args.regions || [];
    const list = choices.map(String);
    // Keep a value that is not among the choices selectable.
    if (this.value !== '' && !list.includes(String(this.value))) {
      list.unshift(String(this.value));
    }
    if (!f.required && f.widget !== 'select') list.unshift('');
    return list.map((v) => ({
      value: v,
      label: v === '' ? '(default)' : f.enumLabels[v] || v,
    }));
  }

  @action change(value) {
    this.args.onChange(this.field, value);
  }

  @action changeNumber(text) {
    this.args.onChange(this.field, text === '' ? null : Number(text));
  }

  @action changeText(text) {
    this.args.onDraft(this.field, text);
  }

  @action toggleReveal() {
    this.revealed = !this.revealed;
  }
}
