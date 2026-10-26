/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import {
  fetchRegionServers,
  leaderName,
  summarizeRaft,
} from 'nomad-ui/utils/servers-data';

const members = [
  {
    Name: 'srv-1.eu-west',
    Addr: '10.0.0.1',
    Status: 'alive',
    Tags: { region: 'eu-west', port: '4647' },
  },
  {
    Name: 'srv-2.eu-west',
    Addr: '10.0.0.2',
    Status: 'failed',
    Tags: { region: 'eu-west', port: '4647' },
  },
  {
    Name: 'srv-3',
    Addr: 'fe80::1',
    Status: 'alive',
    Tags: { region: 'us-east', port: '4647' },
  },
];

function respond(status, body) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    statusText: '',
    json: () => Promise.resolve(body),
  });
}

module('Unit | Utility | servers-data', function () {
  test('leaderName finds the member by its RPC address and drops the region suffix', function (assert) {
    const servers = { members };
    assert.equal(leaderName('10.0.0.2:4647', servers), 'srv-2');
    assert.equal(leaderName('[fe80::1]:4647', servers), 'srv-3');
    assert.equal(
      leaderName('10.9.9.9:4647', servers),
      '10.9.9.9:4647',
      'an unknown leader keeps its address'
    );
    assert.equal(leaderName('', servers), null, 'no leader');
  });

  test('summarizeRaft counts the peers and the voters', function (assert) {
    const raft = summarizeRaft({
      Servers: [
        { Address: '10.0.0.1:4647', Voter: true },
        { Address: '10.0.0.2:4647', Voter: false },
      ],
    });
    assert.equal(raft.peers, 2);
    assert.equal(raft.voters, 1);
    assert.false(raft.byAddress['10.0.0.2:4647'].Voter);
  });

  test('fetchRegionServers summarizes a region, without the Raft peers when they are forbidden', async function (assert) {
    const urls = [];
    const request = (url) => {
      urls.push(url);
      if (url.startsWith('/v1/status/leader'))
        return respond(200, '10.0.0.1:4647');
      return respond(403, null);
    };

    const summary = await fetchRegionServers(request, {
      region: 'eu-west',
      members,
    });

    assert.deepEqual(urls, [
      '/v1/status/leader?region=eu-west',
      '/v1/operator/raft/configuration?region=eu-west',
    ]);
    assert.ok(summary.reachable);
    assert.equal(summary.leaderName, 'srv-1');
    assert.equal(summary.servers.total, 2, 'only the members of the region');
    assert.equal(summary.servers.alive, 1);
    assert.equal(summary.raft, null);
    assert.ok(summary.raftForbidden);
  });

  test('fetchRegionServers marks a region that does not answer as unreachable', async function (assert) {
    const summary = await fetchRegionServers(() => respond(500, null), {
      region: 'us-east',
      members,
    });
    assert.notOk(summary.reachable);
    assert.ok(summary.error);
    assert.equal(summary.servers.total, 1);
  });

  test('fetchRegionServers sends no region query param when queryRegion is null', async function (assert) {
    const urls = [];
    const request = (url) => {
      urls.push(url);
      return respond(200, url.startsWith('/v1/status/leader') ? '' : null);
    };

    const summary = await fetchRegionServers(request, {
      region: 'eu-west',
      members,
      queryRegion: null,
    });

    assert.deepEqual(urls, [
      '/v1/status/leader',
      '/v1/operator/raft/configuration',
    ]);
    assert.equal(summary.region, 'eu-west');
  });
});
