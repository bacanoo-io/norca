/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { tokenAbility } from 'nomad-ui/utils/token-ability';
import { allowsSecret } from 'nomad-ui/utils/norca-acl';

// The built-in secret store. Listing (paths and metadata, no values) on
// any path of any namespace opens the Secrets page; each action on the
// page is checked again for its own namespace and path.
export default tokenAbility({
  list: (token) => allowsSecret(token, '*', '*', 'list-secrets'),
});
