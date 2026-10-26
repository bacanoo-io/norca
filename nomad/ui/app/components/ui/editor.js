/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { task } from 'ember-concurrency';
import { validate, parseJSON } from 'nomad-ui/utils/json-schema';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';
import localStorageProperty from 'nomad-ui/utils/properties/local-storage';

/**
 * The unified editor (UI plan, common UX patterns): HCL or JSON with
 * validation, and before every modification a preview and a diff, modeled
 * on `job plan`.
 *
 *   <Ui::Editor
 *     @label="Policy rules"
 *     @language="hcl"
 *     @value={{this.rules}} @onChange={{this.setRules}}
 *     @original={{this.policy.rules}}
 *     @validate={{this.check}}
 *     @onSubmit={{this.save}} @submitLabel="Save policy"
 *     @onCancel={{this.cancel}}
 *   />
 *
 * Steps: edit, then "Review changes" checks the text (JSON syntax and
 * @schema locally; @validate(text, language) remotely, returning a list of
 * { message, line }), runs @onReview(text, language) whose result is the
 * preview, and shows the review: the :review block (receiving the preview;
 * e.g. the job plan) or a line diff against @original. "Submit" calls
 * @onSubmit(text, language, preview). Errors of each step are shown with
 * the editor.
 *
 * @languages lists the formats the user can switch between
 * (@onLanguageChange); @readOnly shows the code without the steps;
 * @noReview submits directly (for drafts that change nothing yet).
 */
export default class UiEditor extends Component {
  @tracked stage = 'edit';
  @tracked preview = null;
  @tracked remoteProblems = [];
  @tracked submitError = null;

  @localStorageProperty('nomadShouldWrapCode', false) wrap;

  get language() {
    return this.args.language || 'hcl';
  }

  get languageLabel() {
    return this.language.toUpperCase();
  }

  get languageOptions() {
    return (this.args.languages || []).map((value) => ({
      value,
      label: value.toUpperCase(),
    }));
  }

  get text() {
    return this.args.value ?? '';
  }

  // Problems found locally: JSON syntax and the JSON schema.
  get localProblems() {
    if (this.language !== 'json' || !this.text.trim()) return [];
    const parsed = parseJSON(this.text);
    if (parsed.error) {
      return [
        {
          message: `Invalid JSON: ${parsed.error.message}`,
          line: parsed.error.line,
        },
      ];
    }
    if (!this.args.schema) return [];
    return validate(this.args.schema, parsed.value).map((e) => ({
      message: `${e.path}: ${e.message}`,
    }));
  }

  get problems() {
    return [...this.localProblems, ...this.remoteProblems];
  }

  get errorLines() {
    return this.problems.map((p) => p.line).filter(Boolean);
  }

  get isEmpty() {
    return !this.text.trim();
  }

  get isUnchanged() {
    return (
      this.args.original !== undefined &&
      this.args.original !== null &&
      this.text === this.args.original
    );
  }

  get canReview() {
    return (
      !this.isEmpty && !this.localProblems.length && !this.review.isRunning
    );
  }

  @action
  change(text) {
    this.remoteProblems = [];
    this.submitError = null;
    this.args.onChange?.(text);
  }

  @action
  toggleWrap() {
    this.wrap = !this.wrap;
  }

  @action
  setLanguage(language) {
    this.remoteProblems = [];
    this.args.onLanguageChange?.(language);
  }

  @task({ drop: true })
  *review() {
    this.remoteProblems = [];
    this.submitError = null;
    if (this.localProblems.length) return;
    try {
      if (this.args.validate) {
        const problems = yield this.args.validate(this.text, this.language);
        if (problems && problems.length) {
          this.remoteProblems = problems;
          return;
        }
      }
      this.preview = this.args.onReview
        ? yield this.args.onReview(this.text, this.language)
        : null;
      if (this.args.noReview) {
        yield this.submit.perform();
        return;
      }
      this.stage = 'review';
    } catch (err) {
      this.remoteProblems = [
        {
          message: messageFromAdapterError(
            err,
            this.args.permission || 'make this change'
          ),
        },
      ];
    }
  }

  @task({ drop: true })
  *submit() {
    this.submitError = null;
    try {
      yield this.args.onSubmit?.(this.text, this.language, this.preview);
      if (!this.isDestroying && !this.isDestroyed) {
        this.stage = 'edit';
        this.preview = null;
      }
    } catch (err) {
      this.submitError = messageFromAdapterError(
        err,
        this.args.permission || 'make this change'
      );
    }
  }

  @action
  backToEdit() {
    this.stage = 'edit';
    this.preview = null;
    this.submitError = null;
  }
}
