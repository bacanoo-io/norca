/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { waitForPromise } from '@ember/test-waiters';

// window.fetch, resolved at call time (Mirage/Pretender replaces it while a
// test or a development mock server runs) and registered with the test
// waiters so that settled() waits for pending requests, like ember-fetch did.
export default function fetchToUse(...args) {
  return waitForPromise(window.fetch(...args));
}
