/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsBlock, allowsNamespace } from 'nomad-ui/utils/norca-acl';
import {
  healthSummary,
  listExternalServices,
  saveExternalService,
  deleteExternalService,
  listNodes,
} from 'nomad-ui/utils/networking';

const CAPABILITY = 'register-external-service';

const UNITS = { ns: 1, us: 1e3, µs: 1e3, ms: 1e6, s: 1e9, m: 60e9, h: 3600e9 };

// parseDuration reads a Go duration ("30s", "1h30m"); null when invalid.
export function parseDuration(text) {
  const s = String(text ?? '').trim();
  if (s === '') return null;
  if (s === '0') return 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let total = 0;
  let consumed = 0;
  let m;
  while ((m = re.exec(s))) {
    if (m.index !== consumed) return null;
    total += parseFloat(m[1]) * UNITS[m[2]];
    consumed = re.lastIndex;
  }
  return consumed === s.length ? Math.round(total) : null;
}

// formatDuration writes nanoseconds as a Go duration ("1h30m", "10s").
export function formatDuration(ns) {
  if (!ns) return '';
  const parts = [];
  let rest = ns;
  for (const [unit, size] of [
    ['h', 3600e9],
    ['m', 60e9],
    ['s', 1e9],
    ['ms', 1e6],
  ]) {
    const n = Math.floor(rest / size);
    if (n) {
      parts.push(`${n}${unit}`);
      rest -= n * size;
    }
  }
  return parts.join('') || `${ns}ns`;
}

function newCheck(type = 'ttl') {
  return {
    Type: type,
    TTL: '30s',
    HTTP: '',
    Method: 'GET',
    TCP: '',
    Interval: '10s',
    Timeout: '2s',
  };
}

/**
 * Endpoints and domains: external services registered without a job, with
 * an address (an endpoint) or as a DNS alias (a domain: the built-in DNS
 * answers a CNAME). They appear in the service catalog, the DNS and the
 * Consul-compatible API like the services of allocations. TTL checks are
 * renewed by the registrant; http and tcp checks run on the chosen client
 * node.
 */
export default class EndpointsPage extends Component {
  @service token;
  @service frame;
  @service system;
  @service eventStream;

  @tracked services = [];
  @tracked nodes = [];
  @tracked isLoading = true;
  @tracked error = null;
  @tracked editing = null;
  @tracked isSaving = false;
  @tracked saveError = null;
  @tracked deleting = null;
  @tracked isDeleting = false;
  @tracked deleteError = null;

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get baseNamespace() {
    return this.namespace === '*' ? 'default' : this.namespace;
  }

  get scopeLabel() {
    return `${this.region || 'Local region'}, ${
      this.namespace === '*' ? 'all namespaces' : `namespace ${this.namespace}`
    }`;
  }

  get canWrite() {
    return allowsNamespace(this.token, this.namespace, CAPABILITY);
  }

  get canBindNode() {
    return allowsBlock(this.token, 'Node', 'write');
  }

  get nodeOptions() {
    return [
      { value: '', label: 'None' },
      ...this.nodes
        .filter((n) => n.Status === 'ready')
        .map((n) => ({
          value: n.ID,
          label: `${n.Name} (${n.Datacenter})`,
        })),
    ];
  }

  nodeName(id) {
    return this.nodes.find((n) => n.ID === id)?.Name || id?.slice(0, 8);
  }

  get rows() {
    return this.services
      .map((s) => {
        const alias = s.External?.Kind === 'alias';
        const checks = s.External?.Checks || [];
        return {
          ...s,
          key: `${s.Namespace}/${s.ID}`,
          isAlias: alias,
          target: alias
            ? s.Address
            : s.Port
            ? `${s.Address}:${s.Port}`
            : s.Address,
          health: healthSummary([s]),
          checkTypes: checks.map((c) => c.Type).join(', ') || 'static',
          owner: s.NodeID ? this.nodeName(s.NodeID) : '',
          fromConsul: s.External?.Source === 'consul',
          canWrite:
            s.External?.Source !== 'consul' &&
            allowsNamespace(this.token, s.Namespace, CAPABILITY),
        };
      })
      .sort((a, b) => a.ServiceName.localeCompare(b.ServiceName));
  }

  get totals() {
    const rows = this.rows;
    return {
      total: rows.length,
      domains: rows.filter((r) => r.isAlias).length,
      critical: rows.filter((r) => r.Health === 'critical').length,
    };
  }

  async load() {
    try {
      const { body, index } = await listExternalServices(
        this.token,
        this.region,
        this.namespace
      );
      this.services = body || [];
      this.error = null;
      return index;
    } catch (e) {
      this.error =
        e.status === 403
          ? 'Your token cannot read the services of this namespace (read-job is needed).'
          : errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  async loadNodes() {
    try {
      const { body } = await listNodes(this.token, this.region);
      this.nodes = body || [];
    } catch {
      // without node read the owner column shows node IDs
      this.nodes = [];
    }
  }

  setup = modifier((element, [region, namespace]) => {
    waitForPromise(Promise.all([this.load(), this.loadNodes()]));
    const unsubscribe = this.eventStream.subscribe({
      topics: ['Service'],
      region,
      namespace,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  @action
  add() {
    this.saveError = null;
    this.editing = {
      isNew: true,
      Namespace: this.baseNamespace,
      Name: '',
      Kind: 'address',
      Address: '',
      Port: '',
      Target: '',
      Datacenter: '',
      Tags: '',
      Meta: '',
      NodeID: '',
      DeregisterCriticalAfter: '1h',
      Checks: [],
    };
  }

  @action
  edit(row) {
    this.saveError = null;
    const ext = row.External || {};
    this.editing = {
      isNew: false,
      ID: row.ID,
      Namespace: row.Namespace,
      Name: row.ServiceName,
      Kind: ext.Kind || 'address',
      Address: ext.Kind === 'alias' ? '' : row.Address || '',
      Port: row.Port ? String(row.Port) : '',
      Target: ext.Kind === 'alias' ? row.Address || '' : '',
      Datacenter: row.Datacenter || '',
      Tags: (row.Tags || []).join(', '),
      Meta: Object.entries(row.Meta || {})
        .map(([k, v]) => `${k}=${v}`)
        .join('\n'),
      NodeID: row.NodeID || '',
      DeregisterCriticalAfter: formatDuration(ext.DeregisterCriticalAfter),
      Checks: (ext.Checks || []).map((c) => ({
        ID: c.ID,
        Type: c.Type,
        TTL: formatDuration(c.TTL) || '30s',
        HTTP: c.HTTP || '',
        Method: c.Method || 'GET',
        TCP: c.TCP || '',
        Interval: formatDuration(c.Interval) || '10s',
        Timeout: formatDuration(c.Timeout) || '2s',
      })),
    };
  }

  @action
  setField(field, value) {
    this.editing = { ...this.editing, [field]: value };
  }

  @action
  addCheck() {
    this.editing = {
      ...this.editing,
      Checks: [...this.editing.Checks, newCheck()],
    };
  }

  @action
  removeCheck(index) {
    this.editing = {
      ...this.editing,
      Checks: this.editing.Checks.filter((_, i) => i !== index),
    };
  }

  @action
  setCheckField(index, field, value) {
    const checks = this.editing.Checks.map((c, i) =>
      i === index ? { ...c, [field]: value } : c
    );
    this.editing = { ...this.editing, Checks: checks };
  }

  get needsNode() {
    return (this.editing?.Checks || []).some((c) => c.Type !== 'ttl');
  }

  get kindOptions() {
    return [
      { value: 'address', label: 'Endpoint (address)' },
      { value: 'alias', label: 'Domain (DNS alias)' },
    ];
  }

  get checkTypeOptions() {
    return [
      { value: 'ttl', label: 'TTL' },
      { value: 'http', label: 'HTTP' },
      { value: 'tcp', label: 'TCP' },
    ];
  }

  // body builds the native API registration of the editor, or throws a
  // message.
  body() {
    const e = this.editing;
    const name = e.Name.trim();
    if (!/^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$/.test(name)) {
      throw 'The name must be a DNS label: letters, digits and dashes, at most 63 characters.';
    }
    const svc = {
      Name: name,
      Kind: e.Kind,
      Datacenter: e.Datacenter.trim() || undefined,
      NodeID: e.NodeID || undefined,
    };
    if (!e.isNew) svc.ID = e.ID;
    if (e.Kind === 'alias') {
      if (!e.Target.trim()) throw 'A domain needs the DNS name it points to.';
      svc.Target = e.Target.trim();
    } else {
      if (!e.Address.trim()) throw 'An endpoint needs an address.';
      svc.Address = e.Address.trim();
    }
    if (String(e.Port).trim() !== '') {
      const port = Number(e.Port);
      if (!Number.isInteger(port) || port < 1 || port > 65535) {
        throw 'The port must be between 1 and 65535.';
      }
      svc.Port = port;
    }
    const tags = e.Tags.split(',')
      .map((t) => t.trim())
      .filter(Boolean);
    if (tags.length) svc.Tags = tags;
    const meta = {};
    for (const line of e.Meta.split('\n')) {
      if (!line.trim()) continue;
      const i = line.indexOf('=');
      if (i < 1) throw `Meta lines are key=value: "${line.trim()}".`;
      meta[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
    if (Object.keys(meta).length) svc.Meta = meta;

    const dca = parseDuration(e.DeregisterCriticalAfter);
    if (e.DeregisterCriticalAfter.trim() !== '' && dca === null) {
      throw 'Deregister after critical is a duration such as 1h or 30m, or 0.';
    }
    svc.DeregisterCriticalAfter = dca === null ? undefined : dca;

    svc.Checks = e.Checks.map((c) => {
      const check = { Type: c.Type };
      if (c.ID) check.ID = c.ID;
      if (c.Type === 'ttl') {
        check.TTL = parseDuration(c.TTL);
        if (!check.TTL) throw 'A TTL check needs a TTL such as 30s.';
        return check;
      }
      check.Interval = parseDuration(c.Interval);
      check.Timeout = parseDuration(c.Timeout);
      if (!check.Interval || !check.Timeout) {
        throw 'HTTP and TCP checks need an interval and a timeout such as 10s and 2s.';
      }
      if (c.Type === 'http') {
        if (!/^https?:\/\//.test(c.HTTP.trim())) {
          throw 'An HTTP check needs an http:// or https:// URL.';
        }
        check.HTTP = c.HTTP.trim();
        check.Method = c.Method || 'GET';
      } else {
        if (!c.TCP.trim()) throw 'A TCP check needs a host:port.';
        check.TCP = c.TCP.trim();
      }
      return check;
    });
    if (this.needsNode && !e.NodeID) {
      throw 'HTTP and TCP checks run on a client node: choose the node.';
    }
    return svc;
  }

  @action
  closeEditor() {
    if (!this.isSaving) this.editing = null;
  }

  @action
  async save(event) {
    event?.preventDefault();
    let svc;
    try {
      svc = this.body();
    } catch (message) {
      this.saveError = String(message);
      return;
    }
    this.isSaving = true;
    this.saveError = null;
    try {
      await saveExternalService(
        this.token,
        this.region,
        this.editing.Namespace,
        svc
      );
      this.editing = null;
      await this.load();
    } catch (err) {
      this.saveError = errorMessage(err);
    } finally {
      this.isSaving = false;
    }
  }

  @action
  askDelete(row) {
    this.deleteError = null;
    this.deleting = row;
  }

  @action
  cancelDelete() {
    this.deleting = null;
  }

  @action
  async confirmDelete() {
    this.isDeleting = true;
    this.deleteError = null;
    try {
      await deleteExternalService(
        this.token,
        this.region,
        this.deleting.Namespace,
        this.deleting.ID
      );
      this.deleting = null;
      await this.load();
    } catch (e) {
      this.deleteError = errorMessage(e);
    } finally {
      this.isDeleting = false;
    }
  }
}
