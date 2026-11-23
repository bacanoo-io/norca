/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Route from '@ember/routing/route';

// A stack of Rigger: the releases it owns and its operations.
export default class PackagesStackRoute extends Route {
  model({ namespace, name }) {
    return { namespace, name };
  }
}
