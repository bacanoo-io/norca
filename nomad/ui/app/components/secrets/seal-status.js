/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsBlock } from 'nomad-ui/utils/norca-acl';
import { sealStatus } from 'nomad-ui/utils/secrets';
import { summarizeSeal } from 'nomad-ui/utils/dashboard-data';

/**
 * The key protection of a region (GET /v1/operator/seal, operator read):
 * the seals that wrap the keyring and, per server, whether it is sealed
 * and the state of the secret store core. Unsealing stays in the CLI
 * (norca operator unseal): shares are never typed into a web page.
 */
export default class SecretsSealStatus extends Component {
  @service token;

  @tracked status = null;
  @tracked error = null;
  @tracked isLoading = true;

  get allowed() {
    return allowsBlock(this.token, 'Operator', 'read');
  }

  get summary() {
    return summarizeSeal(this.status);
  }

  get servers() {
    return (this.status?.Servers || []).map((s) => {
      let store = 'active';
      let storeTone = 'success';
      if (s.SecretCoreRecovery) {
        store = 'needs recovery';
        storeTone = 'danger';
      } else if (!s.SecretCoreInitialized) {
        store = 'not initialized';
        storeTone = 'neutral';
      } else if (s.SecretCoreSealed) {
        store = 'sealed';
        storeTone = 'danger';
      } else if (!s.SecretCoreActive) {
        store = 'standby';
        storeTone = 'neutral';
      }
      return {
        ...s,
        sealLabel: s.Error
          ? 'unknown'
          : s.Sealed
          ? 'sealed'
          : s.Initialized === false
          ? 'not initialized'
          : 'unsealed',
        sealTone: s.Error ? 'warning' : s.Sealed ? 'danger' : 'success',
        progress:
          s.Sealed && s.Threshold
            ? `${s.Progress || 0} of ${s.Threshold} shares entered`
            : '',
        store,
        storeTone,
        seals: (s.Seals || []).join(', ') || '–',
      };
    });
  }

  async load(region) {
    if (!this.allowed) {
      this.isLoading = false;
      return;
    }
    try {
      this.status = (await sealStatus(this.token, region)).body;
      this.error = null;
    } catch (e) {
      this.error = e.status === 403 ? null : errorMessage(e);
      this.status = null;
    } finally {
      this.isLoading = false;
    }
  }

  setup = modifier((element, [region]) => {
    waitForPromise(Promise.resolve().then(() => this.load(region)));
  });
}
