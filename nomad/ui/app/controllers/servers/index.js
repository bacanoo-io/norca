/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { alias } from '@ember/object/computed';
import Controller, { inject as controller } from '@ember/controller';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { waitForPromise } from '@ember/test-waiters';
import Sortable from 'nomad-ui/mixins/sortable';
import WithListActions from 'nomad-ui/mixins/with-list-actions';
import { fetchRegionServers, membersOf } from 'nomad-ui/utils/servers-data';

export default class IndexController extends Controller.extend(
  Sortable,
  WithListActions
) {
  @service router;
  @service system;
  @service token;

  @controller('servers') serversController;
  @alias('serversController.isForbidden') isForbidden;

  @alias('model.nodes') nodes;
  @alias('model.agents') agents;

  queryParams = [
    {
      currentPage: 'page',
    },
    {
      sortProperty: 'sort',
    },
    {
      sortDescending: 'desc',
    },
  ];

  currentPage = 1;
  pageSize = 8;

  sortProperty = 'isLeader';
  sortDescending = true;

  @alias('agents') listToSort;
  @alias('listSorted') sortedAgents;

  columns = [
    { key: 'name', label: 'Name', sortable: true, hideable: false },
    { key: 'status', label: 'Status', sortable: true },
    { key: 'isLeader', label: 'Role', sortable: true },
    { key: 'region', label: 'Region', sortable: true },
    { key: 'address', label: 'Address', sortable: true },
    { key: 'serfPort', label: 'Port', sortable: true },
    { key: 'datacenter', label: 'Datacenter', sortable: true },
    { key: 'raft', label: 'Raft' },
    { key: 'version', label: 'Version', sortable: true },
  ];

  /** One summary per region (see utils/servers-data), sorted by region. */
  @tracked regionSummaries = [];
  @tracked regionsLoading = false;

  get regionNames() {
    const regions = (this.system.regions || []).slice().sort();
    return regions.length ? regions : [''];
  }

  // The region of a server; servers without a region tag belong to the
  // agent's region.
  regionOf = (agent) => {
    return agent.region || this.system.get('defaultRegion.region') || '';
  };

  summaryOf(agent) {
    const region = this.regionOf(agent);
    return (
      this.regionSummaries.find((s) => s.region === region) ||
      (this.regionSummaries.length === 1 ? this.regionSummaries[0] : null)
    );
  }

  colorIndexOf = (region) => {
    const index = this.regionNames.indexOf(region);
    return index < 0 ? 0 : (index % 6) + 1;
  };

  get regionRows() {
    return this.regionSummaries.map((summary) => ({
      ...summary,
      colorIndex: this.colorIndexOf(summary.region),
    }));
  }

  // Whether the server leads its region: the leader of every region is
  // known from its summary; until then the active region's leader.
  isLeader = (agent) => {
    const summary = this.summaryOf(agent);
    if (summary?.reachable) return summary.leader === agent.rpcAddr;
    return agent.isLeader;
  };

  raftPeerOf = (agent) => {
    return this.summaryOf(agent)?.raft?.byAddress?.[agent.rpcAddr] || null;
  };

  async loadRegions() {
    this.regionsLoading = true;
    try {
      const members = membersOf(this.agents?.toArray?.() || []);
      const request = (url) => this.token.authorizedRawRequest(url);
      // Like every other request, a single-region cluster is queried
      // without the region query param.
      const single = this.regionNames.length === 1;
      this.regionSummaries = await Promise.all(
        this.regionNames.map((region) =>
          fetchRegionServers(request, {
            region,
            members,
            queryRegion: single ? null : region,
          })
        )
      );
    } finally {
      this.regionsLoading = false;
    }
  }

  @action
  refreshRegions() {
    return waitForPromise(this.loadRegions());
  }

  @action
  gotoServer(agent) {
    this.router.transitionTo('servers.server', agent.id);
  }
}
