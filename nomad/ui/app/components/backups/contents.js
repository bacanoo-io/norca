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
import {
  backupContents,
  CONTENT_TYPES,
  itemKey,
  itemLabel,
} from 'nomad-ui/utils/backups';

const shortId = (id) => String(id || '').split('-')[0];
const fromNs = (ns) => (ns ? new Date(ns / 1e6).toLocaleString() : '–');

// Columns and cells of each content type. A cell is { value, mono, status }.
const VIEWS = {
  jobs: {
    columns: ['Job', 'Namespace', 'Type', 'Status', 'Version', 'Versions kept'],
    cells: (j) => [
      { value: j.ID, mono: true },
      { value: j.Namespace },
      { value: j.Type },
      { status: j.Stop ? 'stopped' : j.Status },
      { value: `v${j.Version}` },
      { value: (j.Versions || []).map((v) => `v${v}`).join(', ') || '–' },
    ],
    item: (j) => ({ Type: 'job', Namespace: j.Namespace, ID: j.ID }),
  },
  'job-versions': {
    columns: ['Version', 'Namespace', 'Type', 'Status', 'Submitted'],
    cells: (j) => [
      { value: `v${j.Version}` },
      { value: j.Namespace },
      { value: j.Type },
      { status: j.Stop ? 'stopped' : j.Status },
      { value: fromNs(j.SubmitTime) },
    ],
    item: (j) => ({
      Type: 'job',
      Namespace: j.Namespace,
      ID: j.ID,
      Version: j.Version,
    }),
  },
  deployments: {
    columns: [
      'Deployment',
      'Job',
      'Version',
      'Status',
      'Healthy',
      'Description',
    ],
    cells: (d) => {
      const groups = Object.values(d.TaskGroups || {});
      const desired = groups.reduce((s, g) => s + (g.DesiredTotal || 0), 0);
      const healthy = groups.reduce((s, g) => s + (g.HealthyAllocs || 0), 0);
      return [
        { value: shortId(d.ID), mono: true },
        { value: d.JobID },
        { value: `v${d.JobVersion}` },
        { status: d.Status },
        { value: `${healthy}/${desired}` },
        { value: d.StatusDescription || '–' },
      ];
    },
  },
  allocations: {
    columns: [
      'Allocation',
      'Name',
      'Job version',
      'Client',
      'Status',
      'Desired',
    ],
    cells: (a) => [
      { value: shortId(a.ID), mono: true },
      { value: a.Name },
      { value: `v${a.JobVersion}` },
      { value: a.NodeName || shortId(a.NodeID) },
      { status: a.ClientStatus },
      { value: a.DesiredStatus },
    ],
  },
  variables: {
    columns: ['Path', 'Namespace', 'Keys (values are never shown)'],
    cells: (v) => [
      { value: v.Path, mono: true },
      { value: v.Namespace },
      { value: (v.Keys || []).join(', ') || '–' },
    ],
    item: (v) => ({ Type: 'variable', Namespace: v.Namespace, ID: v.Path }),
  },
  namespaces: {
    columns: ['Namespace', 'Description', 'Quota'],
    cells: (n) => [
      { value: n.Name, mono: true },
      { value: n.Description || '–' },
      { value: n.Quota || '–' },
    ],
    item: (n) => ({ Type: 'namespace', ID: n.Name }),
  },
  'acl-policies': {
    columns: ['Policy', 'Description'],
    cells: (p) => [
      { value: p.Name, mono: true },
      { value: p.Description || '–' },
    ],
    item: (p) => ({ Type: 'acl-policy', ID: p.Name }),
  },
  'acl-roles': {
    columns: ['Role', 'Description', 'Policies'],
    cells: (r) => [
      { value: r.Name, mono: true },
      { value: r.Description || '–' },
      { value: (r.Policies || []).map((p) => p.Name).join(', ') || '–' },
    ],
    item: (r) => ({ Type: 'acl-role', ID: r.Name }),
  },
};

/**
 * Browsing the content of a backup by type: jobs (with their kept
 * versions), deployments and allocations (the history the user asked to be
 * browsable), Variables paths with key names only, namespaces, ACL policies
 * and roles. Restorable items can be added to a selective restore.
 */
export default class BackupsContents extends Component {
  @service token;
  @service frame;
  @service backups;
  @service router;

  @tracked items = [];
  @tracked isLoading = false;
  @tracked error = null;
  @tracked identityError = null;
  @tracked filter = '';

  get type() {
    const t = this.args.type;
    if (t === 'job-versions' && this.args.job) return t;
    return CONTENT_TYPES.some((c) => c.id === t) ? t : 'jobs';
  }

  get tabs() {
    return CONTENT_TYPES.map((t) => ({ id: t.id, label: t.label }));
  }

  get selectedTab() {
    return this.type === 'job-versions' ? 'jobs' : this.type;
  }

  get view() {
    return VIEWS[this.type];
  }

  get columns() {
    const cols = this.view.columns.map((label, i) => ({
      key: `c${i}`,
      label,
    }));
    if (this.type === 'jobs') cols.push({ key: 'history', label: 'History' });
    if (this.view.item) cols.push({ key: 'select', label: 'Restore' });
    return cols;
  }

  get selection() {
    return this.backups.draftFor(this.args.backup.ID);
  }

  get selectedKeys() {
    return new Set(this.selection.map(itemKey));
  }

  get rows() {
    const q = this.filter.trim().toLowerCase();
    const selected = this.selectedKeys;
    return this.items
      .map((raw) => {
        const cells = this.view.cells(raw);
        const item = this.view.item?.(raw);
        return {
          raw,
          cells,
          item,
          isSelected: item ? selected.has(itemKey(item)) : false,
          label: item ? itemLabel(item) : '',
          text: cells
            .map((c) => c.value || c.status || '')
            .join(' ')
            .toLowerCase(),
        };
      })
      .filter((row) => !q || row.text.includes(q));
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  get emptyMessage() {
    if (this.filter) return 'Nothing matches the filter.';
    const label = (
      CONTENT_TYPES.find((t) => t.id === this.type)?.label || 'items'
    ).toLowerCase();
    if (this.args.job)
      return `The backup has no ${label} of job ${this.args.job}.`;
    return `The backup has no ${label} in the namespaces your token can read.`;
  }

  async load() {
    if (!this.backups.hasIdentity) return;
    this.isLoading = true;
    this.error = null;
    try {
      const params = { type: this.type, namespace: this.namespace };
      if (this.args.job) params.job = this.args.job;
      const { body } = await backupContents(this.token, {
        region: this.args.region,
        id: this.args.backup.ID,
        identity: this.backups.identity,
        ...params,
      });
      const key =
        this.type === 'job-versions'
          ? 'JobVersions'
          : CONTENT_TYPES.find((t) => t.id === this.type).key;
      this.items = body?.[key] || [];
      this.identityError = null;
    } catch (e) {
      const message = errorMessage(e);
      if (/identity|recipient|decrypt/i.test(message)) {
        // a wrong identity: ask again
        this.backups.forgetIdentity();
        this.identityError = message;
      } else {
        this.error = message;
      }
      this.items = [];
    } finally {
      this.isLoading = false;
    }
  }

  loader = modifier((element, [, , , hasIdentity]) => {
    if (hasIdentity) waitForPromise(Promise.resolve().then(() => this.load()));
  });

  @action
  selectType(id) {
    this.filter = '';
    this.args.onParamsChange?.({ type: id, job: null });
  }

  @action
  showJob(type, jobId) {
    this.filter = '';
    this.args.onParamsChange?.({ type, job: jobId });
  }

  @action
  clearJob() {
    this.args.onParamsChange?.({
      type: this.type === 'job-versions' ? 'jobs' : this.type,
      job: null,
    });
  }

  @action
  setFilter(value) {
    this.filter = value;
  }

  @action
  toggle(row) {
    const key = itemKey(row.item);
    const current = this.selection;
    const next = current.some((i) => itemKey(i) === key)
      ? current.filter((i) => itemKey(i) !== key)
      : [...current, row.item];
    this.backups.setDraft(this.args.backup.ID, next);
  }

  @action
  clearSelection() {
    this.backups.clearDraft(this.args.backup.ID);
  }

  @action
  restoreSelected() {
    this.router.transitionTo('backups.restore', this.args.backup.ID, {
      queryParams: { mode: 'selective' },
    });
  }
}
