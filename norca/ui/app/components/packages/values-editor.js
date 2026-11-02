/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { prettyJSON } from 'nomad-ui/utils/norca-editor';
import { valuesSchema, upgradeCommand } from 'nomad-ui/utils/releases';

/**
 * The variable values of a release in the unified editor: the values of
 * the deployed revision as JSON, checked against the variables schema of
 * the pack, and a diff before the upgrade is prepared. Packs are rendered
 * by the client (pack/docs/releases.md), so the result is the CLI command
 * that renders the pack with the new values and applies it as a new
 * revision; secret values are never shown or stored here.
 *
 *   <Packages::ValuesEditor @release={{this.release}} @revision={{this.deployed}}
 *     @onClose={{this.close}} />
 */
export default class PackagesValuesEditor extends Component {
  @tracked text = this.original;
  @tracked command = null;

  get original() {
    return prettyJSON(this.args.revision?.Spec?.Values || {});
  }

  get schema() {
    const schema = valuesSchema(this.args.revision);
    // The values are the overrides: required variables may come from the
    // defaults of the pack.
    if (!schema) return { type: 'object' };
    // eslint-disable-next-line no-unused-vars
    const { required, ...rest } = schema;
    return rest;
  }

  get nextRevision() {
    return (this.args.release?.Status?.Revision || 0) + 1;
  }

  get hasSchema() {
    return !!valuesSchema(this.args.revision);
  }

  @action
  setText(text) {
    this.text = text;
    this.command = null;
  }

  @action
  prepare(text) {
    this.command = upgradeCommand(
      this.args.release,
      this.args.revision,
      JSON.parse(text)
    );
  }
}
