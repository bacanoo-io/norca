/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { errorMessage } from 'nomad-ui/utils/norca-api';

// Helpers of the feature area editors built on Ui::Editor.

/**
 * Turns an ApiError of utils/norca-api into the error shape Ui::Editor
 * reports (adapter style `errors[].detail`), so the agent's message is shown
 * with the editor. A 403 says which permission is missing.
 */
export function editorError(error, permission) {
  const detail =
    error?.status === 403
      ? `Your ACL token does not grant permission to ${permission}.`
      : errorMessage(error) || 'The request failed.';
  const wrapped = new Error(detail);
  wrapped.errors = [{ status: String(error?.status || ''), detail }];
  return wrapped;
}

// Pretty JSON of a value for the editor and the diffs.
export function prettyJSON(value) {
  return JSON.stringify(value ?? null, null, 2);
}

// The value without the Raft indexes and the other fields the server
// manages, for an editable document.
export function withoutFields(value, fields) {
  const copy = { ...(value || {}) };
  fields.forEach((f) => delete copy[f]);
  return copy;
}
