/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import moment from 'moment';
import { phaseLabel } from 'nomad-ui/utils/node-scaling';

function ago(t) {
  if (!t) return '–';
  const m = moment(t);
  return m.isValid() ? m.fromNow() : String(t);
}

function list(v) {
  return Array.isArray(v) ? v : [];
}

/**
 * The page of a norca.io/NodeGroup object: the scale and what the node
 * scaling operator reports, the instances of the provider with their nodes
 * (an instance whose node has not joined is here before its node is in the
 * client list), the removal of a scale down, the rolling replacement, the
 * deletion and the nodes without an instance. @object is the object.
 */
export default class ObjectsNodeGroup extends Component {
  get spec() {
    return this.args.object?.Spec || {};
  }

  get status() {
    return this.args.object?.Status || {};
  }

  get summary() {
    const sp = this.spec;
    const st = this.status;
    const items = [
      { title: 'Scale', value: sp.replicas ?? sp.min ?? '–' },
      { title: 'Ready', value: st.replicas ?? 'not reported' },
      { title: 'Joining', value: st.joining ?? 0 },
      { title: 'Failed', value: st.failed ?? 0 },
      { title: 'Min / max', value: `${sp.min ?? 0} / ${sp.max ?? '–'}` },
      {
        title: 'Where',
        value: `pool ${sp.pool}, datacenter ${sp.datacenter}${
          sp.node_class ? `, class ${sp.node_class}` : ''
        }`,
      },
    ];
    if (sp.provider)
      items.push({
        title: 'Provider',
        value: `${sp.provider}${sp.group ? ` (group ${sp.group})` : ''}`,
      });
    if (sp.enforce === false)
      items.push({
        title: 'Enforce',
        value: 'off: the autoscaler only raises events',
      });
    return items;
  }

  get instances() {
    const st = this.status;
    const replacing = new Map(
      list(st.rollout?.replacing).map((r) => [r.instance, r])
    );
    return list(st.nodes).map((n) => {
      const notes = [];
      if (n.removing) notes.push(`scale down: ${phaseLabel(n.removing)}`);
      const r = replacing.get(n.instance);
      if (r) notes.push(`replaced (${r.reason}): ${phaseLabel(r.phase)}`);
      if (n.slow) notes.push('joining slowly');
      if (n.reason) notes.push(n.reason);
      let tone = 'neutral';
      let label = n.state;
      if (n.state === 'failed') tone = 'danger';
      else if (n.node && n.ready) {
        tone = 'success';
        label = 'ready';
      } else if (n.node) {
        tone = 'warning';
        label = 'down';
      } else if (n.state === 'creating' || n.state === 'running') {
        tone = 'info';
        label = 'joining';
      }
      return {
        instance: n.instance,
        node: n.node || null,
        shortNode: n.node ? n.node.slice(0, 8) : null,
        request: n.request,
        created: ago(n.created),
        tone,
        label,
        notes: notes.join('; '),
      };
    });
  }

  get removal() {
    const m = this.status.removal;
    if (!m) return null;
    return {
      ...m,
      phaseLabel: phaseLabel(m.phase),
      since: ago(m.started),
    };
  }

  get rollout() {
    const ro = this.status.rollout;
    if (!ro) return null;
    return {
      replacing: list(ro.replacing).map((r) => ({
        ...r,
        phaseLabel: phaseLabel(r.phase),
        since: ago(r.since),
        how: r.surge ? 'new node first' : 'removed first',
      })),
      outdated: list(ro.outdated),
      backoff: ro.backoff || null,
      retry: ro.backoff?.retryAfter ? ago(ro.backoff.retryAfter) : null,
    };
  }

  get deletion() {
    return this.status.deletion || null;
  }

  get strays() {
    return list(this.status.strayNodes).map((s) => ({
      ...s,
      shortNode: s.node ? s.node.slice(0, 8) : '',
      label: s.kind === 'foreign' ? 'unverified' : 'instance gone',
      tone: s.kind === 'foreign' ? 'danger' : 'neutral',
    }));
  }
}
