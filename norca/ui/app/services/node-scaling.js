/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service, { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { listObjects } from 'nomad-ui/utils/objects';
import { NODE_GROUP, nodeIndex } from 'nomad-ui/utils/node-scaling';

/**
 * The NodeGroup objects of the active region and the index of their nodes
 * (utils/node-scaling), shared by the client list and the client page. A
 * cluster without the node scaling kinds, or a token that can't read them,
 * gets an empty index: the pages show no badges then.
 */
export default class NodeScalingService extends Service {
  @service token;
  @service system;

  @tracked groups = [];
  @tracked index = nodeIndex([]);
  @tracked loaded = false;

  loadedAt = 0;
  loading = null;

  /** Loads the groups, at most every maxAge milliseconds. */
  refresh(maxAge = 10000) {
    if (this.loading) return this.loading;
    if (this.loaded && Date.now() - this.loadedAt < maxAge) {
      return Promise.resolve(this.index);
    }
    this.loading = this.load().finally(() => {
      this.loading = null;
    });
    return this.loading;
  }

  async load() {
    let groups = [];
    try {
      const { body } = await listObjects(
        this.token,
        this.system.activeRegion || '',
        NODE_GROUP.group,
        NODE_GROUP.kind
      );
      groups = Array.isArray(body) ? body : [];
    } catch {
      groups = [];
    }
    this.groups = groups;
    this.index = nodeIndex(groups);
    this.loaded = true;
    this.loadedAt = Date.now();
    return this.index;
  }

  /** The NodeGroup object of a name, from the last load. */
  group(name) {
    return this.groups.find((g) => g.Name === name) || null;
  }
}
