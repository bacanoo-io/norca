/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// The nodes of the node scaling operator (norca-nodescaler) as the UI shows
// them. What a node is (autoscaled, joining, being removed or replaced,
// outdated, unverified) comes from the status of its NodeGroup object:
// status.nodes, status.removal, status.rollout and status.strayNodes. The
// meta of a node only names the group to look in.

export const NODE_GROUP = { group: 'norca.io', kind: 'NodeGroup' };

/** The meta keys of a node the operator created. */
export const META = {
  createdBy: 'norca.created_by',
  group: 'norca.node_group',
  instance: 'norca.instance',
  createdAt: 'norca.created_at',
  revision: 'norca.node_group_revision',
  generation: 'norca.node_group_generation',
  certExpires: 'norca.cert_expires',
  replace: 'norca.scale.replace',
  protect: 'norca.scale.protect',
  leaving: 'norca.lb.leaving',
};

const PHASES = {
  leaving: 'Leaving the load balancers',
  draining: 'Draining',
  settling: 'Checked for work',
  deleting: 'Deleting its instance',
  deleted: 'Instance deleted',
  surging: 'Waiting for its replacement',
  emptying: 'Waiting until empty',
};

function list(v) {
  return Array.isArray(v) ? v : [];
}

/**
 * The index of the nodes of the node groups. groups are NodeGroup objects
 * (as the objects API returns them). Returns
 *
 *   byNode:  node ID -> { group, instance, badges: [{label, color, title}] }
 *   groups:  [{ name, nodes, joining }] sorted by name, for the filter
 *   joining: group name -> [instances whose node has not joined]
 */
export function nodeIndex(groups) {
  const byNode = new Map();
  const summary = [];
  const joining = {};
  for (const g of groups || []) {
    const name = g.Name;
    const st = g.Status || {};
    const removal = st.removal || null;
    const replacing = new Map(
      list(st.rollout?.replacing).map((r) => [r.instance, r])
    );
    const outdated = new Set(list(st.rollout?.outdated));
    const ids = [];
    joining[name] = [];
    for (const rec of list(st.nodes)) {
      if (!rec.node) {
        if (rec.state === 'creating' || rec.state === 'running')
          joining[name].push(rec);
        continue;
      }
      const badges = [
        {
          label: 'Autoscaled',
          color: 'azure',
          title: `Created by the node scaling operator for node group ${name} (instance ${rec.instance})`,
        },
      ];
      if (rec.removing) {
        badges.push({
          label: 'Removing',
          color: 'orange',
          title: `Removed by a scale down: ${
            PHASES[rec.removing] || rec.removing
          }`,
        });
      }
      const rep = replacing.get(rec.instance);
      if (rep) {
        badges.push({
          label: 'Replacing',
          color: 'purple',
          title: `Replaced (${rep.reason}): ${PHASES[rep.phase] || rep.phase}`,
        });
      }
      if (rec.slow) {
        badges.push({
          label: 'Slow join',
          color: 'yellow',
          title: 'Joined after join.slow_after',
        });
      }
      // Outdated nodes are listed by name in the rollout.
      byNode.set(rec.node, {
        group: name,
        instance: rec.instance,
        outdated,
        badges,
      });
      ids.push(rec.node);
    }
    for (const s of list(st.strayNodes)) {
      if (!s.node) continue;
      byNode.set(s.node, {
        group: name,
        instance: s.instance,
        badges: [
          s.kind === 'foreign'
            ? {
                label: 'Unverified',
                color: 'red',
                title: `Carries the marks of node group ${name} (instance ${s.instance}) without a machine the operator asked for; it gets no new work`,
              }
            : {
                label: 'Instance gone',
                color: 'secondary',
                title: `Down, and its instance ${s.instance} of node group ${name} is gone`,
              },
        ],
      });
      ids.push(s.node);
    }
    if (removal?.node && !ids.includes(removal.node)) ids.push(removal.node);
    summary.push({ name, nodes: ids, joining: joining[name].length });
  }
  summary.sort((a, b) => a.name.localeCompare(b.name));
  return { byNode, groups: summary, joining };
}

/**
 * The badges of a node: those of the index, Outdated when the rollout of its
 * group lists its name, and Unverified for a node with the marks of a group
 * that the group does not know.
 */
export function nodeBadges(index, node, meta) {
  const entry = index?.byNode.get(node.id);
  if (entry) {
    const badges = [...entry.badges];
    if (entry.outdated?.has(node.name)) {
      badges.push({
        label: 'Outdated',
        color: 'yellow',
        title: 'The rollout of its node group will replace it',
      });
    }
    return badges;
  }
  const group = meta?.[META.group];
  if (group && meta?.[META.createdBy] === 'nodescaler' && index) {
    return [
      {
        label: 'Unverified',
        color: 'red',
        title: `Carries the marks of node group ${group}, which does not list it`,
      },
    ];
  }
  return [];
}

/** The node group of a node, by the index or by its meta. */
export function nodeGroupOf(index, node, meta) {
  return index?.byNode.get(node.id)?.group || meta?.[META.group] || null;
}

/**
 * A duration as the NodeGroup schema writes them ("45d", "720h", "1h30m")
 * in milliseconds; 0 when it can't be read.
 */
export function parseDuration(s) {
  if (!s || typeof s !== 'string') return 0;
  const units = { s: 1e3, m: 6e4, h: 3.6e6, d: 8.64e7 };
  let total = 0;
  let rest = s;
  const re = /^(\d+)([smhd])/;
  while (rest.length) {
    const m = re.exec(rest);
    if (!m) return 0;
    total += Number(m[1]) * units[m[2]];
    rest = rest.slice(m[0].length);
  }
  return total;
}

/** FNV-1a, 32 bit, of the UTF-8 bytes of s: the hash of the operator. */
export function fnv32a(s) {
  let h = 0x811c9dc5;
  for (const b of new TextEncoder().encode(s)) {
    h ^= b;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}

/** The fixed share of the jitter of a node, in [0, 1). */
export function jitterOf(name) {
  return (fnv32a(name) % 10000) / 10000;
}

/**
 * When the operator replaces a node by its age, as it computes it:
 *
 *   soft: the soft limit, rolling.max_node_age (or three quarters of the
 *         life of its certificate) less the jitter of the node;
 *   hard: the hard limit, a twelfth of the life of its certificate before
 *         it expires.
 *
 * meta is the meta of the node, spec the spec of its group (may be null),
 * name the name of its instance. Returns dates or null.
 */
export function replacementTimes(meta, spec, name) {
  const created = Date.parse(meta?.[META.createdAt] || '');
  const expires = Date.parse(meta?.[META.certExpires] || '');
  const out = { created: null, expires: null, soft: null, hard: null };
  if (!Number.isNaN(created)) out.created = new Date(created);
  if (!Number.isNaN(expires)) out.expires = new Date(expires);
  if (out.created && out.expires) {
    const life = expires - created;
    if (life > 0) out.hard = new Date(expires - life / 12);
  }
  let maxAge = parseDuration(spec?.rolling?.max_node_age);
  if (!maxAge && out.created && out.expires) {
    maxAge = ((expires - created) * 3) / 4;
  }
  if (maxAge > 0 && out.created) {
    let jitter = 0.15;
    const m = /^(\d{1,2})%$/.exec(spec?.rolling?.jitter || '');
    if (m) jitter = Number(m[1]) / 100;
    out.soft = new Date(created + maxAge * (1 - jitter * jitterOf(name || '')));
  }
  return out;
}

/** A readable phase of a removal or a replacement. */
export function phaseLabel(phase) {
  return PHASES[phase] || phase || '';
}
