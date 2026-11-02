/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// Data of the servers and regions page: per region the leader, the server
// members, the Raft peers and whether the region answers (the federation
// connection). With several regions every region is queried explicitly
// with ?region= through the token service's raw request, like the
// dashboard; a single-region cluster is queried without it.

import { summarizeServers, withParams } from 'nomad-ui/utils/dashboard-data';
import formatHost from 'nomad-ui/utils/format-host';

async function getJSON(request, path, params) {
  const response = await request(withParams(path, params));
  if (!response.ok) {
    const error = new Error(
      `${path}: ${response.status} ${response.statusText || ''}`.trim()
    );
    error.status = response.status;
    throw error;
  }
  return response.json();
}

// The name of the leader (its RPC address) among the members; the region
// suffix of the member name is left out.
export function leaderName(leader, servers) {
  if (!leader) return null;
  const member = servers.members.find(
    (m) => formatHost(m.Addr || m.Address, m.Tags?.port) === leader
  );
  if (!member) return leader;
  const suffix = member.Tags?.region ? `.${member.Tags.region}` : null;
  return suffix && member.Name.endsWith(suffix)
    ? member.Name.slice(0, -suffix.length)
    : member.Name;
}

// The members of the agent store records in the shape of /v1/agent/members.
export function membersOf(agents = []) {
  return agents.map((agent) => ({
    Name: agent.name,
    Addr: agent.address,
    Status: agent.status,
    Tags: { region: agent.region, port: agent.rpcPort, dc: agent.datacenter },
  }));
}

// The Raft peers of a region: total, voters, and the peer IDs by address.
export function summarizeRaft(configuration) {
  const servers = configuration?.Servers || [];
  return {
    peers: servers.length,
    voters: servers.filter((s) => s.Voter).length,
    byAddress: Object.fromEntries(servers.map((s) => [s.Address, s])),
  };
}

// queryRegion is the region query param (default: the region; null sends
// none).
export async function fetchRegionServers(
  request,
  { region, members, queryRegion = region }
) {
  const servers = summarizeServers(members, region);
  let leader = null;
  try {
    leader = await getJSON(request, '/v1/status/leader', {
      region: queryRegion,
    });
  } catch (e) {
    return {
      region,
      reachable: false,
      error: e.message,
      leader: null,
      leaderName: null,
      servers,
      raft: null,
      raftForbidden: false,
    };
  }

  let raft = null;
  let raftForbidden = false;
  try {
    raft = summarizeRaft(
      await getJSON(request, '/v1/operator/raft/configuration', {
        region: queryRegion,
      })
    );
  } catch (e) {
    // The Raft configuration needs operator:read; without it the rest of
    // the region is still shown.
    if (e.status === 403) raftForbidden = true;
  }

  return {
    region,
    reachable: true,
    error: null,
    leader: leader || null,
    leaderName: leaderName(leader, servers),
    servers,
    raft,
    raftForbidden,
  };
}
