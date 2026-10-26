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
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import { allowsBlock, allowsSecret } from 'nomad-ui/utils/norca-acl';
import {
  getSecretMetadata,
  readSecret,
  writeSecret,
  secretVersionsOp,
  secretMeta,
  shortHash,
  formatInterval,
  accessorLabel,
  findReferencingJobs,
  JOB_SCAN_LIMIT,
} from 'nomad-ui/utils/secrets';

const time = (ns) =>
  ns ? moment(ns / 1e6).format('YYYY-MM-DD HH:mm:ss') : '–';

/**
 * A secret of the built-in store: its metadata, the current value (revealed
 * on an explicit click), the version history (metadata and value hash),
 * per-version delete, undelete, destroy and restore as a new version, the
 * jobs that reference the path and the recent audit records. Writes are
 * conditional on the check index of the loaded secret.
 */
export default class SecretsDetail extends Component {
  @service token;
  @service system;
  @service eventStream;
  @service router;
  @service notifications;

  @tracked secret = null;
  @tracked isLoading = true;
  @tracked loadError = null;

  @tracked revealVersion = 0;
  @tracked writing = false;
  @tracked editingMeta = false;

  // the pending versions operation: { op, version, title, ... }
  @tracked pending = null;
  @tracked pendingError = null;
  @tracked isRunning = false;

  @tracked references = null;
  @tracked referencesError = null;
  @tracked accesses = [];

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.args.namespace || 'default';
  }

  allows(capability) {
    return allowsSecret(this.token, this.namespace, this.args.path, capability);
  }

  get canRead() {
    return this.allows('read-secret');
  }

  get canWrite() {
    return this.allows('write-secret');
  }

  get canDestroy() {
    return this.allows('destroy-secret');
  }

  get canAudit() {
    return allowsBlock(this.token, 'Operator', 'read');
  }

  get state() {
    const s = this.secret;
    if (!s) return null;
    if (s.Destroyed) return { label: 'destroyed', tone: 'danger' };
    if (s.Deleted) return { label: 'deleted', tone: 'warning' };
    return { label: 'active', tone: 'success' };
  }

  get expired() {
    const at = this.secret?.ExpiresAt;
    return at && new Date(at).getTime() < Date.now();
  }

  get metadata() {
    const s = this.secret;
    if (!s) return [];
    const items = [
      { title: 'Namespace', value: s.Namespace },
      { title: 'Description', value: s.Description || '–' },
      { title: 'Owner', value: s.Owner || '–' },
      {
        title: 'Current version',
        value: `v${s.CurrentVersion} (keeps the last ${s.MaxVersions || 10})`,
      },
      {
        title: 'Items',
        value: (s.ItemKeys || []).join(', ') || '–',
        mono: true,
      },
      {
        title: 'Value hash',
        value: s.ValueHash ? shortHash(s.ValueHash) : '–',
        mono: true,
      },
      { title: 'Created', value: time(s.CreateTime) },
      { title: 'Updated', value: time(s.ModifyTime) },
    ];
    if (s.ExpiresAt)
      items.push({
        title: 'Expires',
        value: `${moment(s.ExpiresAt).format('YYYY-MM-DD HH:mm')} (${moment(
          s.ExpiresAt
        ).fromNow()})`,
      });
    if (s.RotateEvery)
      items.push({
        title: 'Rotate every',
        value: formatInterval(s.RotateEvery),
      });
    return items;
  }

  get labels() {
    return Object.entries(this.secret?.Labels || {}).map(
      ([k, v]) => `${k}=${v}`
    );
  }

  get versions() {
    const s = this.secret;
    if (!s) return [];
    return (s.Versions || [])
      .slice()
      .sort((a, b) => b.Version - a.Version)
      .map((v) => {
        const isCurrent = v.Version === s.CurrentVersion;
        const state = v.Destroyed
          ? 'destroyed'
          : v.Deleted
          ? 'deleted'
          : isCurrent
          ? 'current'
          : 'kept';
        return {
          ...v,
          isCurrent,
          state,
          tone: v.Destroyed
            ? 'danger'
            : v.Deleted
            ? 'warning'
            : isCurrent
            ? 'success'
            : 'neutral',
          created: time(v.CreateTime),
          hash: shortHash(v.ValueHash),
          readable: !v.Deleted && !v.Destroyed,
          canRestore: !isCurrent && !v.Deleted && !v.Destroyed,
        };
      });
  }

  get revealed() {
    const version = this.revealVersion || this.secret?.CurrentVersion;
    return this.versions.find((v) => v.Version === version) || null;
  }

  get accessRows() {
    return this.accesses.slice(0, 10).map((r) => ({
      when: moment(r.time).fromNow(),
      operation: r.Operation,
      version: r.Version || '',
      who: accessorLabel(r.Accessor),
      result: r.Result || '',
      tone: r.Result === 'ok' || !r.Result ? 'success' : 'danger',
    }));
  }

  async load() {
    try {
      const { body } = await getSecretMetadata(
        this.token,
        this.region,
        this.namespace,
        this.args.path
      );
      this.secret = body;
      this.loadError = null;
    } catch (e) {
      this.secret = null;
      this.loadError =
        e.status === 404
          ? 'The secret does not exist. It may have been destroyed, or it is in another namespace or region.'
          : e.status === 403
          ? 'Your token cannot see this secret: it needs list-secrets (or read-secret) on the path.'
          : errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  async loadReferences() {
    try {
      this.references = await findReferencingJobs(
        this.token,
        this.region,
        this.namespace,
        this.args.path
      );
      this.referencesError = null;
    } catch (e) {
      this.references = null;
      this.referencesError =
        e.status === 403
          ? 'Your token cannot list the jobs of the namespace.'
          : errorMessage(e);
    }
  }

  get referencesNote() {
    const r = this.references;
    if (!r || r.total <= r.scanned) return null;
    return `Only the first ${JOB_SCAN_LIMIT} of ${r.total} jobs of the namespace were checked.`;
  }

  // --- reveal, write, metadata -----------------------------------------

  @action
  showVersion(version) {
    this.revealVersion = version;
  }

  @action openWrite() {
    this.writing = true;
  }

  @action closeWrite() {
    this.writing = false;
  }

  @action
  async written() {
    this.writing = false;
    this.revealVersion = 0;
    await this.load();
  }

  @action openMeta() {
    this.editingMeta = true;
  }

  @action closeMeta() {
    this.editingMeta = false;
  }

  @action
  async metaSaved() {
    this.editingMeta = false;
    await this.load();
  }

  // --- versions operations ---------------------------------------------

  @action
  ask(op, version) {
    const path = this.args.path;
    const v = version ? `version ${version}` : 'all versions';
    const texts = {
      delete: {
        title: `Delete version ${version}?`,
        label: 'Delete version',
        text: `Version ${version} of ${path} is soft deleted: tasks can no longer read it, and it can be undeleted until it is destroyed.`,
      },
      undelete: {
        title: `Undelete version ${version}?`,
        label: 'Undelete',
        tone: 'primary',
        text: `Version ${version} of ${path} becomes readable again.`,
      },
      destroy: {
        title: version
          ? `Destroy version ${version}?`
          : `Destroy the secret ${path}?`,
        label: version ? 'Destroy version' : 'Destroy secret',
        typed: version ? `v${version}` : path,
        text: version
          ? `The value of version ${version} is removed permanently; only its metadata remains. This cannot be undone.`
          : `The secret is removed permanently with ${v}. This cannot be undone; tasks that use it fail on their next read.`,
      },
      restore: {
        title: `Restore version ${version} as a new version?`,
        label: 'Restore',
        tone: 'primary',
        text: `The value of version ${version} is read (an audited read) and written as version ${
          (this.secret?.CurrentVersion || 0) + 1
        }. The history stays as it is.`,
      },
    };
    this.pendingError = null;
    this.pending = { op, version, ...texts[op] };
  }

  @action
  closePending() {
    this.pending = null;
  }

  @action
  async runPending() {
    const { op, version } = this.pending;
    const s = this.secret;
    this.isRunning = true;
    this.pendingError = null;
    try {
      if (op === 'restore') {
        await this.restore(version);
      } else {
        const { body } = await secretVersionsOp(
          this.token,
          this.region,
          this.namespace,
          s.Path,
          op,
          { versions: version ? [version] : [], cas: s.ModifyIndex }
        );
        if (op === 'destroy' && !version) {
          this.pending = null;
          this.notifications.add({
            title: `Secret ${s.Path} destroyed`,
            color: 'success',
          });
          this.router.transitionTo('secrets.index');
          return;
        }
        if (body?.Secret) this.secret = body.Secret;
      }
      this.notifications.add({
        title:
          op === 'restore'
            ? `Version ${version} restored as version ${this.secret?.CurrentVersion}`
            : `Version ${version} ${
                {
                  delete: 'deleted',
                  undelete: 'undeleted',
                  destroy: 'destroyed',
                }[op]
              }`,
        color: 'success',
      });
      this.pending = null;
      if (this.revealVersion === version) this.revealVersion = 0;
      await this.load();
    } catch (e) {
      const capability = {
        delete: 'write-secret',
        undelete: 'write-secret',
        destroy: 'destroy-secret',
        restore: 'read-secret and write-secret',
      }[op];
      this.pendingError =
        e.status === 403
          ? `Your token lacks ${capability} on this path.`
          : e.status === 409
          ? 'The secret changed since the page loaded it. Close this dialog; the page shows the current state.'
          : errorMessage(e);
      if (e.status === 409) await this.load();
    } finally {
      this.isRunning = false;
    }
  }

  // Reads the value of a kept version and writes it as a new version,
  // conditional on the loaded secret. The value is held in this function
  // only.
  async restore(version) {
    const s = this.secret;
    const { body } = await readSecret(
      this.token,
      this.region,
      this.namespace,
      s.Path,
      version
    );
    const { body: written } = await writeSecret(
      this.token,
      this.region,
      this.namespace,
      s.Path,
      { items: body?.Items || {}, cas: s.ModifyIndex }
    );
    if (written?.Secret) this.secret = written.Secret;
  }

  get meta() {
    return secretMeta(this.secret);
  }

  setup = modifier((element, [region, namespace, path]) => {
    waitForPromise(
      Promise.resolve().then(() =>
        Promise.all([this.load(), this.loadReferences()])
      )
    );
    const unsubscribers = [];
    // The Secret topic, keyed by the path: allowed with list-secrets on it.
    if (allowsSecret(this.token, namespace, path, 'list-secrets')) {
      unsubscribers.push(
        this.eventStream.subscribe({
          topics: [`Secret:${path}`],
          region,
          namespace,
          onEvents: () => waitForPromise(this.load()),
          onResync: () => this.load(),
        })
      );
    }
    if (this.canAudit) {
      unsubscribers.push(
        this.eventStream.subscribe({
          topics: ['SecretAccess'],
          region,
          namespace,
          replay: true,
          onEvents: (events) => {
            const mine = events
              .filter(
                (e) =>
                  e.Topic === 'SecretAccess' &&
                  (e.Payload?.Path || e.Key) === path
              )
              .map((e) => ({
                ...e.Payload,
                time: e.Payload?.Time ? new Date(e.Payload.Time).getTime() : 0,
              }));
            if (mine.length)
              this.accesses = [...mine, ...this.accesses].sort(
                (a, b) => b.time - a.time
              );
          },
        })
      );
    }
    return () => unsubscribers.forEach((u) => u());
  });
}
