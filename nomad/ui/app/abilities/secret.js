/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from './abstract';
import { allowsSecret } from 'nomad-ui/utils/norca-acl';

// The built-in secret store: list-secrets (paths and metadata) on any path
// of any namespace shows the Secrets page. The pages check the namespace
// and path of each action with utils/norca-acl.allowsSecret.
export default class Secret extends AbstractAbility {
  get canList() {
    return allowsSecret(this.token, '*', '*', 'list-secrets');
  }
}
