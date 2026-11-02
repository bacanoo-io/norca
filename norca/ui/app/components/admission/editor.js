/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { apiRequest } from 'nomad-ui/utils/norca-api';
import { editorError } from 'nomad-ui/utils/norca-editor';
import {
  ADMISSION_SCHEMA,
  CEL_REFERENCE,
  CEL_EXAMPLES,
  admissionPolicyText,
} from 'nomad-ui/utils/admission';

/**
 * The CEL editor of an admission policy, in the unified editor: the JSON
 * form of the policy checked against its schema, a diff before the write,
 * and the CEL compile errors of the server shown with the editor.
 *
 *   <Admission::Editor @policy={{this.policy}} @region={{this.region}}
 *     @onSaved={{this.saved}} @onClose={{this.close}} />
 *
 * Without @policy it creates a policy from a template.
 */
export default class AdmissionEditor extends Component {
  @service token;
  @service notifications;

  @tracked text = admissionPolicyText(this.args.policy);

  schema = ADMISSION_SCHEMA;
  reference = CEL_REFERENCE;
  examples = CEL_EXAMPLES.map((code, i) => ({
    code,
    label: `Example rule ${i + 1}`,
  }));

  get isNew() {
    return !this.args.policy;
  }

  get original() {
    return this.isNew ? '' : admissionPolicyText(this.args.policy);
  }

  get title() {
    return this.isNew
      ? 'New admission policy'
      : `Edit admission policy ${this.args.policy.Name}`;
  }

  @action
  setText(text) {
    this.text = text;
  }

  // A renamed policy would be a new one: the name of an existing policy
  // stays.
  @action
  validate(text) {
    const doc = JSON.parse(text);
    if (!this.isNew && doc.Name !== this.args.policy.Name) {
      return [
        {
          message: `Name: an existing policy cannot be renamed; keep "${this.args.policy.Name}" or create a new policy.`,
        },
      ];
    }
    return [];
  }

  @action
  async submit(text) {
    const doc = JSON.parse(text);
    try {
      await apiRequest(
        this.token,
        `/v1/admission-policy/${encodeURIComponent(doc.Name)}`,
        {
          method: 'PUT',
          params: { region: this.args.region || undefined },
          data: doc,
        }
      );
    } catch (e) {
      throw editorError(e, 'write admission policies (admission write)');
    }
    this.notifications.add({
      title: `Admission policy ${doc.Name} saved`,
      message:
        (doc.Action || 'deny') === 'warn'
          ? 'Writes that break its rules go through with a warning.'
          : 'Writes that break its rules are rejected.',
      color: 'success',
    });
    this.args.onSaved?.(doc.Name);
  }
}
