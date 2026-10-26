/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { withParams } from 'nomad-ui/utils/dashboard-data';

// Requests of the pages of the new feature areas (backups, quotas,
// packages, objects, ...). They go through the token service's raw request
// with an explicit ?region= (a page can show another region than the
// active one) and return the decoded body with the X-Nomad-Index.

export class ApiError extends Error {
  constructor(message, status, body) {
    super(message);
    this.status = status;
    this.body = body;
  }

  get isForbidden() {
    return this.status === 403;
  }

  get isNotFound() {
    return this.status === 404;
  }
}

export { withParams };

async function decode(response) {
  const text = await response.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (e) {
    return text;
  }
}

// An error message for the UI: the agent's error text when it has one.
export function errorMessage(error) {
  if (!error) return '';
  const body = error.body;
  if (typeof body === 'string' && body.trim()) return body.trim();
  if (body && typeof body === 'object') {
    return body.Error || body.error || body.message || error.message;
  }
  return error.message || String(error);
}

/**
 * Sends a request and decodes the JSON answer.
 *
 *   const { body, index } = await apiRequest(this.token, '/v1/backups', {
 *     params: { region: 'eu' },
 *   });
 *   await apiRequest(this.token, '/v1/backup', { method: 'PUT', data: {...} });
 *
 * Throws ApiError (with status and the decoded body) on a non-2xx answer.
 */
export async function apiRequest(
  token,
  path,
  { method = 'GET', params = {}, data } = {}
) {
  const init = { method };
  if (data !== undefined) {
    init.body = JSON.stringify(data);
  }
  const url = withParams(path, params);
  const response = await token.authorizedRawRequest(url, init);
  const body = await decode(response);
  if (!response.ok) {
    throw new ApiError(
      `${method} ${path}: ${response.status} ${
        response.statusText || ''
      }`.trim(),
      response.status,
      body
    );
  }
  const index = Number(response.headers?.get?.('X-Nomad-Index')) || 0;
  return { body, index };
}
