/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Helpers of the acceptance tests of the new feature areas.

import { waitUntil } from '@ember/test-helpers';

// Signs in with a new token. With rulesJSON the token gets one policy with
// those parsed rules (as the agent returns them); type "management" passes
// every check.
export function useToken(rulesJSON, type = 'client') {
  const token = server.create('token', { type });
  if (rulesJSON) {
    const policy = server.create('policy', {
      id: `policy-${token.id}`,
      name: `policy-${token.id}`,
      rulesJSON,
    });
    token.policyIds = [policy.id];
    token.save();
  }
  window.localStorage.nomadTokenSecret = token.secretId;
  return token;
}

// The latest handled request with the method whose URL contains fragment.
export function lastRequest(method, fragment) {
  return server.pretender.handledRequests
    .filter((r) => r.method === method && r.url.includes(fragment))
    .pop();
}

export function lastBody(method, fragment) {
  const request = lastRequest(method, fragment);
  return request ? JSON.parse(request.requestBody) : undefined;
}

// The texts an ECharts chart drew (SVG renderer). ECharts loads lazily
// outside the test waiters, so this waits until the chart has drawn text.
export async function chartTexts(selector) {
  const texts = () =>
    [...document.querySelectorAll(`${selector} svg text`)].map((t) =>
      t.textContent.trim()
    );
  await waitUntil(() => texts().length > 0, { timeout: 5000 });
  return texts();
}
