/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Component from '@glimmer/component';

/**
 * Review stage of the job editor: shows the plan (diff, scheduler dry-run,
 * warnings and preemptions) and lets the user run or cancel.
 *
 * Receives `data` and `fns` from the parent JobEditor component.
 *
 * @class JobEditorReview
 * @extends Component
 */
export default class JobEditorReview extends Component {
  /**
   * Warnings returned by the plan request, if any.
   *
   * @returns {string|null}
   */
  get warnings() {
    const warnings = this.args.data?.planOutput?.warnings;
    if (typeof warnings !== 'string') {
      return null;
    }
    return warnings.trim() || null;
  }
}
