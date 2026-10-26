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
import formatHost from 'nomad-ui/utils/format-host';
import {
  serviceInstances,
  listIntentions,
  healthSummary,
  inDiscovery,
} from 'nomad-ui/utils/networking';

const ROLES = ['startup', 'readiness', 'liveness'];

/**
 * A service: its instances per region in scope with address, allocation,
 * the startup, readiness and liveness checks, whether discovery returns
 * them, and the intentions that name the service.
 */
export default class ServicesDetail extends Component {
  @service token;
  @service frame;
  @service eventStream;

  @tracked regions = [];
  @tracked intentions = [];
  @tracked isLoading = true;

  get namespace() {
    return this.args.namespace || 'default';
  }

  get scopedRegions() {
    return this.frame.scopedRegions;
  }

  get isAllRegions() {
    return this.scopedRegions.length > 1;
  }

  get columns() {
    const columns = [{ key: 'instance', label: 'Instance' }];
    if (this.isAllRegions) columns.push({ key: 'region', label: 'Region' });
    columns.push(
      { key: 'address', label: 'Address' },
      ...ROLES.map((r) => ({ key: r, label: r[0].toUpperCase() + r.slice(1) })),
      { key: 'health', label: 'Health' },
      { key: 'discovery', label: 'In discovery' }
    );
    return columns;
  }

  get instances() {
    return this.regions.flatMap((r) =>
      (r.instances || []).map((i) => {
        const checks = i.Checks || [];
        return {
          ...i,
          region: r.region,
          shortAlloc: i.AllocID ? i.AllocID.split('-')[0] : '',
          hostPort: formatHost(i.Address, i.Port) || i.Address || '–',
          roleChecks: ROLES.map((role) => {
            const c = checks.filter((x) => x.Role === role);
            if (!c.length) return null;
            const bad = c.find((x) => x.Status !== 'passing');
            return {
              status: bad ? bad.Status : 'passing',
              output: (bad || c[0]).Output,
            };
          }),
          discoverable: inDiscovery(i),
        };
      })
    );
  }

  get summary() {
    return healthSummary(this.regions.flatMap((r) => r.instances || []));
  }

  get otherChecks() {
    return this.instances.flatMap((i) =>
      (i.Checks || [])
        .filter((c) => !c.Role)
        .map((c) => ({ ...c, instance: i.ID }))
    );
  }

  get relatedIntentions() {
    const name = this.args.name;
    return this.intentions
      .filter((i) => i.Destination === name || i.Source === name)
      .map((i) => ({
        ...i,
        direction: i.Destination === name ? 'incoming' : 'outgoing',
      }));
  }

  async load() {
    const regions = await Promise.all(
      this.scopedRegions.map(async (region) => {
        try {
          const { body, index } = await serviceInstances(
            this.token,
            region,
            this.args.name,
            this.namespace
          );
          return { region, instances: body || [], index };
        } catch (e) {
          return { region, instances: [], error: errorMessage(e) };
        }
      })
    );
    let intentions = [];
    try {
      intentions =
        (
          await listIntentions(
            this.token,
            this.scopedRegions[0],
            this.namespace
          )
        ).body || [];
    } catch (e) {
      intentions = [];
    }
    if (this.isDestroying || this.isDestroyed) return;
    this.regions = regions;
    this.intentions = intentions;
    this.isLoading = false;
  }

  setup = modifier((element, [name, regions, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribers = regions.map((region) =>
      this.eventStream.subscribe({
        topics: ['Service', 'ServiceIntention'],
        region,
        namespace,
        onEvents: (events) => {
          if (
            events.some(
              (e) =>
                e.Topic === 'ServiceIntention' ||
                e.Payload?.Service?.ServiceName === name
            )
          )
            waitForPromise(this.load());
        },
        onResync: () => this.load(),
      })
    );
    return () => unsubscribers.forEach((u) => u());
  });
}
