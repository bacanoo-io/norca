/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { isTesting } from '@ember/debug';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import { apiRequest, errorMessage } from 'nomad-ui/utils/norca-api';
import { normalize, phaseTone } from 'nomad-ui/utils/releases';
import {
  cancelRiggerOperation,
  isFinishedOperation,
  listRiggerOperations,
  listRiggerRegistries,
  riggerOperationLog,
  riggerStackBackend,
  riggerStackRequest,
  stackOfSource,
} from 'nomad-ui/utils/rigger';

// How long a read of the log of a running operation waits for records.
const LOG_WAIT = '10s';

// The records of the log the page keeps, the newest.
const MAX_LOG_LINES = 5000;

// How long the page waits before it reads again when the servers cannot
// read the log (they answer at once then).
const UNAVAILABLE_PAUSE_MS = 5000;

/**
 * A stack of Rigger (design/rig-integration.md 4.8d): the releases it owns,
 * their status and drift, its operations (an apply or a destroy the
 * servers run) with the log of one followed while it runs, and an apply
 * again and a destroy of a published stack.
 */
// What an operation did to a release: the action, and whether it failed
// there or skipped the release.
function outcomeText(outcome) {
  if (outcome.failed) return `${outcome.action} (failed)`;
  if (outcome.skipped) return `${outcome.action} (skipped)`;
  return outcome.action;
}

export default class PackagesRiggerStack extends Component {
  @service token;
  @service system;
  @service notifications;

  @tracked releases = [];
  @tracked operations = [];
  @tracked registries = [];
  @tracked isLoading = true;
  @tracked loadError = null;

  // The operation chosen on the page, before the query names it.
  @tracked selectedName = null;

  @tracked log = [];
  @tracked logNote = null;
  @tracked isFollowing = false;

  @tracked status = null;
  @tracked statusError = null;
  @tracked isCheckingStatus = false;

  @tracked applying = false;
  @tracked toDestroy = false;
  @tracked isDestroying = false;
  @tracked actionError = null;

  // The generation of the following of a log: a new one stops the old.
  followGeneration = 0;

  setup = modifier(() => {
    this.load();
    return () => {
      this.followGeneration++;
    };
  });

  get region() {
    return this.system.activeRegion || '';
  }

  // What the stack is, by the owner its releases record.
  get owner() {
    return this.releases.find((r) => r.Spec?.Owner)?.Spec?.Owner || null;
  }

  get source() {
    return this.owner?.Source || this.operation?.spec?.source || '';
  }

  get version() {
    return this.owner?.Version || this.operation?.spec?.version || '';
  }

  // The registry and name of the published stack, to apply it again.
  get published() {
    return stackOfSource(this.registries, this.source);
  }

  get backend() {
    const p = this.published;
    return p
      ? riggerStackBackend({
          token: this.token,
          registry: p.registry,
          stack: p.stack,
        })
      : null;
  }

  get releaseRows() {
    const drift = new Map(
      (this.status?.Releases || []).map((s) => [
        `${s.revision?.release?.namespace}/${s.revision?.release?.name}`,
        s.drift || [],
      ])
    );
    return this.releases.map((r) => {
      const d = drift.get(`${r.Namespace}/${r.Name}`);
      return {
        name: r.Name,
        namespace: r.Namespace,
        module: `${r.Spec?.Pack?.Name || '?'} ${
          r.Spec?.Pack?.Version || ''
        }`.trim(),
        part: r.Spec?.Owner?.Part || '',
        revision: r.Status?.Revision,
        phase: r.Status?.Phase || 'pending',
        phaseTone: phaseTone(r.Status?.Phase),
        drift:
          d === undefined ? '' : d.length ? `${d.length} drifted` : 'in sync',
      };
    });
  }

  // The operation the page shows: the one of the query, else the newest.
  get operation() {
    const named = (name) =>
      name && this.operations.find((o) => o.name === name);
    return (
      named(this.selectedName) ||
      named(this.args.operation) ||
      this.operations[0] ||
      null
    );
  }

  get operationTone() {
    const phase = this.operation?.status?.phase;
    if (phase === 'Succeeded') return 'success';
    if (phase === 'Running') return 'info';
    if (phase === 'Cancelled' || phase === 'Interrupted') return 'warning';
    return 'danger';
  }

  get isRunning() {
    return this.operation?.status?.phase === 'Running';
  }

  get outcomes() {
    const status = this.operation?.status || {};
    return (status.outcomes?.length ? status.outcomes : status.plan || []).map(
      (o) => ({
        ...o,
        text: outcomeText(o),
      })
    );
  }

  async load() {
    this.isLoading = true;
    this.loadError = null;
    try {
      const owner = `stack/${this.args.namespace}/${this.args.name}`;
      const [releases, operations, registries] = await waitForPromise(
        Promise.all([
          apiRequest(this.token, '/v1/releases', {
            params: { region: this.region, namespace: '*', owner },
          }).then((r) => normalize(r.body || [])),
          listRiggerOperations(
            this.token,
            this.region,
            this.args.namespace,
            this.args.name
          ),
          listRiggerRegistries(this.token, this.region)
            .then((r) => r.body)
            .catch(() => []),
        ])
      );
      this.releases = releases.sort((a, b) => a.Name.localeCompare(b.Name));
      this.operations = operations;
      this.registries = registries;
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
    this.followLog();
  }

  // Reads the log of the operation shown, page after page, while it runs.
  async followLog() {
    const op = this.operation;
    const generation = ++this.followGeneration;
    this.log = [];
    this.logNote = null;
    if (!op) return;
    this.isFollowing = true;
    let after = 0;
    try {
      for (;;) {
        const r = await waitForPromise(
          riggerOperationLog(
            this.token,
            this.region,
            this.args.namespace,
            op.name,
            after,
            isFinishedOperation(op.status?.phase) ? undefined : LOG_WAIT
          )
        );
        if (generation !== this.followGeneration) return;
        const page = r.body || {};
        const lines = (page.Lines || []).map((l) => ({
          seq: l.Seq,
          ...this.record(l.Line),
        }));
        if (lines.length) {
          after = lines[lines.length - 1].seq;
          this.log = [...this.log, ...lines].slice(-MAX_LOG_LINES);
        }
        if (page.LogUnavailable) {
          this.logNote = `The log can't be read (${page.LogUnavailable}); the phase is followed.`;
        }
        if (page.Finished) break;
        // An empty page ends the following in tests, whose servers answer
        // at once (as allocations stop polling there).
        if (isTesting() && !lines.length) break;
        if (page.LogUnavailable) {
          await new Promise((resolve) =>
            setTimeout(resolve, UNAVAILABLE_PAUSE_MS)
          );
          if (generation !== this.followGeneration) return;
        }
      }
      // The end of the operation: its outcomes.
      if (op.status?.phase === 'Running') await this.reloadOperations();
    } catch (e) {
      if (generation === this.followGeneration) this.logNote = errorMessage(e);
    } finally {
      if (generation === this.followGeneration) this.isFollowing = false;
    }
  }

  // A record of the log: its time, level and message, the rest as key=value.
  record(line) {
    const { time, level, msg, ...rest } = line || {};
    const fields = Object.entries(rest)
      .map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`)
      .join(' ');
    return {
      time: time ? String(time).replace('T', ' ').replace(/\..*$/, '') : '',
      level: level || '',
      msg: msg || '',
      fields,
      tone:
        level === 'ERROR'
          ? 'text-danger'
          : level === 'WARN'
          ? 'text-warning'
          : '',
    };
  }

  async reloadOperations() {
    this.operations = await waitForPromise(
      listRiggerOperations(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name
      )
    );
  }

  @action selectOperation(name) {
    this.selectedName = name;
    this.args.onOperation?.(name);
    this.followLog();
  }

  @action async cancel() {
    this.actionError = null;
    try {
      await waitForPromise(
        cancelRiggerOperation(
          this.token,
          this.region,
          this.args.namespace,
          this.operation.name
        )
      );
      await this.reloadOperations();
    } catch (e) {
      this.actionError = errorMessage(e);
    }
  }

  @action async checkStatus() {
    this.isCheckingStatus = true;
    this.statusError = null;
    try {
      const r = await waitForPromise(
        riggerStackRequest(
          this.token,
          this.region,
          this.args.namespace,
          'status',
          {
            Source: this.source,
            Version: this.version,
          }
        )
      );
      this.status = r.body;
    } catch (e) {
      this.statusError = errorMessage(e);
    } finally {
      this.isCheckingStatus = false;
    }
  }

  @action openApply() {
    this.applying = true;
  }

  @action closeApply() {
    this.applying = false;
  }

  @action async applied(response) {
    this.applying = false;
    await this.reloadOperations();
    if (response?.Operation) this.selectOperation(response.Operation);
  }

  @action askDestroy() {
    this.toDestroy = true;
  }

  @action cancelDestroy() {
    this.toDestroy = false;
  }

  @action async confirmDestroy() {
    this.isDestroying = true;
    this.actionError = null;
    try {
      const r = await waitForPromise(
        riggerStackRequest(
          this.token,
          this.region,
          this.args.namespace,
          'destroy',
          {
            Source: this.source,
            Version: this.version,
          }
        )
      );
      this.toDestroy = false;
      this.notifications.add({
        title: `The destroy of ${this.args.name} runs as ${r.body?.name}`,
        color: 'success',
      });
      await this.reloadOperations();
      if (r.body?.name) this.selectOperation(r.body.name);
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isDestroying = false;
    }
  }
}
