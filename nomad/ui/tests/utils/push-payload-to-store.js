/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { run } from '@ember/runloop';

// These are private store methods called by store "finder" methods.
// Useful in unit tests when there is store interaction, since calling
// adapter and serializer methods directly will never insert data into
// the store.
export default function pushPayloadToStore(store, payload, modelName) {
  run(() => {
    store._push(payload);
    // Ember Data 4.12 updates live record arrays itself.
    store._didUpdateAll?.(modelName);
  });
}
