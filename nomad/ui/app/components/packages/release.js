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
import { allowsObject } from 'nomad-ui/utils/norca-acl';
import {
  getRelease,
  releaseHistory,
  releaseEvents,
  rollbackRelease,
  testRelease,
  checkDrift,
  uninstallRelease,
  compareRevisions,
  listPackRegistries,
  upsertPackRegistry,
  upgradeTarget,
  registrySuggestion,
  isDrifted,
  signature,
  phaseTone,
  RELEASE_KIND,
} from 'nomad-ui/utils/releases';

/**
 * A release: its resources in apply order, the revision history with
 * comparison of two revisions, hooks and tests, conditions, drift, the
 * state in every region of a multi-region install and the event log.
 * Roll back (with a dry run first), run the tests and uninstall for tokens
 * with write on the release.
 */
export default class PackagesRelease extends Component {
  @service token;
  @service system;
  @service router;
  @service eventStream;
  @service notifications;

  @tracked release = null;
  @tracked history = [];
  @tracked events = [];
  @tracked regionStates = [];
  @tracked isLoading = true;
  @tracked loadError = null;
  @tracked drift = null;
  @tracked busy = null;
  @tracked actionError = null;
  @tracked compareA = null;
  @tracked compareB = null;
  @tracked rollback = null;
  @tracked uninstalling = false;
  @tracked skipHooks = false;
  @tracked editingValues = false;

  get deployedRevision() {
    const n = this.release?.Status?.Revision;
    return this.history.find((r) => r.Revision === n) || null;
  }

  @tracked upgrading = false;

  @action openValues() {
    this.editingValues = true;
  }

  @action closeValues() {
    this.editingValues = false;
  }

  // The pack of the deployed revision, when it came from a cluster
  // registry: the servers can render an upgrade from the catalog.
  // The cluster registries of the region, to upgrade from; null until
  // loaded or without the permission to read them.
  @tracked clusterRegistries = null;
  @tracked addingRegistry = null; // { name, type, source, defaultRef }
  @tracked registryError = null;
  @tracked isAddingRegistry = false;

  get deployedPack() {
    return this.deployedRevision?.Spec?.Pack || this.release?.Spec?.Pack;
  }

  // The cluster registry and pack to upgrade the release from: the
  // registry it was installed from on the servers, or, for releases of
  // the CLI, a cluster registry of the source the CLI recorded.
  get upgradeTarget() {
    return upgradeTarget(this.deployedPack, this.clusterRegistries);
  }

  // A cluster registry to add for a CLI install from a registry the
  // cluster doesn't have.
  get registrySuggestion() {
    if (!this.clusterRegistries || this.upgradeTarget) return null;
    return registrySuggestion(this.deployedPack, this.clusterRegistries);
  }

  async loadRegistries() {
    try {
      const r = await listPackRegistries(this.token, this.region);
      this.clusterRegistries = r.body || [];
    } catch (e) {
      this.clusterRegistries = null;
    }
  }

  @action openAddRegistry() {
    this.registryError = null;
    this.addingRegistry = { ...this.registrySuggestion };
  }

  @action closeAddRegistry() {
    this.addingRegistry = null;
  }

  @action setAddRegistry(field, value) {
    this.addingRegistry = { ...this.addingRegistry, [field]: value };
  }

  @action async confirmAddRegistry() {
    const r = this.addingRegistry;
    this.isAddingRegistry = true;
    this.registryError = null;
    try {
      const res = await upsertPackRegistry(
        this.token,
        this.region,
        r.name.trim(),
        {
          type: r.type,
          source: r.source,
          defaultRef: r.defaultRef.trim() || undefined,
        }
      );
      const status = res.body?.Status || {};
      if (status.Phase === 'Error') {
        this.notifications.add({
          title: `${r.name} added, but the sync failed`,
          message: status.Message,
          color: 'critical',
        });
      } else {
        this.notifications.add({
          title: `${r.name} added to the cluster`,
          color: 'success',
        });
      }
      this.addingRegistry = null;
      await this.loadRegistries();
    } catch (e) {
      this.registryError = errorMessage(e);
    } finally {
      this.isAddingRegistry = false;
    }
  }

  @action openUpgrade() {
    this.upgrading = true;
  }

  @action closeUpgrade() {
    this.upgrading = false;
  }

  @action upgraded() {
    this.upgrading = false;
    this.load();
  }

  get region() {
    return this.system.activeRegion || '';
  }

  get canWrite() {
    return allowsObject(this.token, this.args.namespace, RELEASE_KIND, 'write');
  }

  get phase() {
    return this.release?.Status?.Phase || 'pending';
  }

  get phaseTone() {
    return phaseTone(this.phase);
  }

  get pack() {
    return this.release?.Spec?.Pack || {};
  }

  get signature() {
    return signature(this.release);
  }

  get drifted() {
    return this.drift ? this.drift.Drifted : isDrifted(this.release);
  }

  get driftEntries() {
    return this.drift?.Resources || [];
  }

  get details() {
    const r = this.release;
    if (!r) return [];
    const op = r.Status?.LastOperation;
    return [
      { title: 'Pack', value: `${this.pack.Name} ${this.pack.Version}` },
      { title: 'Registry', value: this.pack.Registry || '–' },
      { title: 'Digest', value: this.pack.Digest || '–', mono: true },
      { title: 'Signature', value: this.signature.title },
      { title: 'Deployed revision', value: r.Status?.Revision ?? '–' },
      {
        title: 'Last operation',
        value: op
          ? `${op.Action} of revision ${op.Revision} by ${
              op.User || 'unknown'
            }${op.Error ? `: ${op.Error}` : ''}`
          : '–',
      },
      {
        title: 'Atomic',
        value: r.Spec?.Atomic ? 'yes: a failure rolls back' : 'no',
      },
    ];
  }

  get resources() {
    return (this.release?.Status?.Resources || []).map((res, i) => ({
      ...res,
      order: i + 1,
    }));
  }

  get conditions() {
    return (this.release?.Status?.Conditions || []).map((c) => ({
      ...c,
      tone:
        c.Type === 'Drifted'
          ? c.Status === 'True'
            ? 'warning'
            : 'success'
          : c.Status === 'True'
          ? 'success'
          : c.Status === 'False'
          ? 'danger'
          : 'neutral',
    }));
  }

  get timeline() {
    return this.history.map((rev) => {
      const s = rev.Spec || {};
      const phase = rev.Status?.Phase;
      const current = rev.Revision === this.release?.Status?.Revision;
      let title = `Revision ${rev.Revision}${current ? ' (deployed)' : ''}`;
      if (s.Action === 'rollback') title += `, rollback to ${s.RollbackOf}`;
      return {
        icon: phase === 'failed' ? 'circle-x' : 'package',
        color: phase === 'failed' ? 'red' : current ? 'green' : 'secondary',
        time: s.CreateTime ? moment(s.CreateTime).fromNow() : '',
        title,
        text: [
          `${s.Action || 'install'} of ${s.Pack?.Name} ${s.Pack?.Version}`,
          s.User && `by ${s.User}`,
          rev.Status?.Message,
        ]
          .filter(Boolean)
          .join(', '),
      };
    });
  }

  get revisionOptions() {
    return this.history.map((r) => ({
      value: String(r.Revision),
      label: `Revision ${r.Revision} (${r.Spec?.Pack?.Version || '?'}, ${
        r.Status?.Phase || ''
      })`,
    }));
  }

  get comparison() {
    const a = this.history.find((r) => String(r.Revision) === this.compareA);
    const b = this.history.find((r) => String(r.Revision) === this.compareB);
    if (!a || !b) return null;
    return compareRevisions(a, b);
  }

  get hooks() {
    const current = this.history.find(
      (r) => r.Revision === this.release?.Status?.Revision
    );
    return current?.Status?.Hooks || [];
  }

  get lastTest() {
    return this.release?.Status?.LastTest || null;
  }

  get eventItems() {
    return this.events
      .slice()
      .sort((a, b) => (b.LastTime || 0) - (a.LastTime || 0))
      .map((e) => ({
        icon: e.Type === 'Warning' ? 'alert-triangle' : 'circle-check',
        color: e.Type === 'Warning' ? 'orange' : 'green',
        title: e.Count > 1 ? `${e.Reason} (${e.Count}×)` : e.Reason,
        text: e.Message,
        time: e.LastTime ? moment(e.LastTime / 1e6).fromNow() : '',
      }));
  }

  get rollbackTargets() {
    const current = this.release?.Status?.Revision;
    return this.history
      .filter(
        (r) =>
          r.Revision !== current &&
          ['deployed', 'superseded'].includes(r.Status?.Phase)
      )
      .map((r) => ({
        value: String(r.Revision),
        label: `Revision ${r.Revision}: ${r.Spec?.Pack?.Name} ${r.Spec?.Pack?.Version}`,
      }));
  }

  async load() {
    const ns = this.args.namespace;
    try {
      const { body } = await getRelease(
        this.token,
        this.region,
        ns,
        this.args.name
      );
      this.release = body;
      this.loadError = null;
    } catch (e) {
      this.loadError =
        e.status === 404
          ? 'The release does not exist in this namespace and region. It may have been uninstalled.'
          : errorMessage(e);
      this.isLoading = false;
      return;
    }
    const [history, events] = await Promise.all([
      releaseHistory(this.token, this.region, ns, this.args.name)
        .then((r) => r.body || [])
        .catch(() => []),
      releaseEvents(this.token, this.region, ns, this.args.name)
        .then((r) => r.body || [])
        .catch(() => []),
    ]);
    this.history = history;
    this.events = events;
    if (this.clusterRegistries === null) await this.loadRegistries();
    if (!this.compareA && history.length > 1) {
      this.compareA = String(history[1].Revision);
      this.compareB = String(history[0].Revision);
    }
    const regions = this.release.Spec?.Multiregion?.Regions || [];
    if (regions.length > 1) {
      this.regionStates = await Promise.all(
        regions.map(async (region) => {
          try {
            const { body } = await getRelease(
              this.token,
              region,
              ns,
              this.args.name
            );
            return {
              region,
              phase: body.Status?.Phase,
              revision: body.Status?.Revision,
              tone: phaseTone(body.Status?.Phase),
              drifted: isDrifted(body),
            };
          } catch (e) {
            return {
              region,
              phase: e.status === 404 ? 'not installed' : 'unreachable',
              tone: 'danger',
            };
          }
        })
      );
    } else {
      this.regionStates = [];
    }
    this.isLoading = false;
  }

  setup = modifier((element, [name, namespace, region]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribe = this.eventStream.subscribe({
      topics: [`CustomObject:${RELEASE_KIND}/${name}`],
      region,
      namespace,
      onEvents: () => waitForPromise(this.load()),
      onResync: () => this.load(),
    });
    return () => unsubscribe();
  });

  @action setCompareA(v) {
    this.compareA = v;
  }

  @action setCompareB(v) {
    this.compareB = v;
  }

  @action dismissError() {
    this.actionError = null;
  }

  @action
  async runDrift() {
    this.busy = 'drift';
    this.actionError = null;
    try {
      const { body } = await checkDrift(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name
      );
      this.drift = body;
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.busy = null;
    }
  }

  @action
  async runTests() {
    this.busy = 'test';
    this.actionError = null;
    try {
      const { body } = await testRelease(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name
      );
      this.notifications.add({
        title: body.Succeeded ? 'The tests passed' : 'A test failed',
        color: body.Succeeded ? 'success' : 'critical',
      });
      await this.load();
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.busy = null;
    }
  }

  @action
  openRollback(revision) {
    const target = revision ? String(revision) : this.rollbackTargets[0]?.value;
    this.rollback = { target, preview: null, error: null, running: false };
    if (target) waitForPromise(this.previewRollback(target));
  }

  @action
  closeRollback() {
    if (!this.rollback?.running) this.rollback = null;
  }

  @action
  setRollbackTarget(value) {
    this.rollback = { ...this.rollback, target: value, preview: null };
    waitForPromise(this.previewRollback(value));
  }

  async previewRollback(target) {
    try {
      const { body } = await rollbackRelease(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name,
        {
          Revision: Number(target),
          DryRun: true,
        }
      );
      if (this.rollback?.target === target)
        this.rollback = {
          ...this.rollback,
          preview: body.Changes || [],
          error: null,
        };
    } catch (e) {
      this.rollback = { ...this.rollback, error: errorMessage(e) };
    }
  }

  @action
  async confirmRollback() {
    this.rollback = { ...this.rollback, running: true, error: null };
    try {
      const { body } = await rollbackRelease(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name,
        {
          Revision: Number(this.rollback.target),
        }
      );
      this.notifications.add({
        title: `Rolled back to revision ${this.rollback.target}`,
        message: `Applied as revision ${body.Revision}.`,
        color: 'success',
      });
      this.rollback = null;
      await this.load();
    } catch (e) {
      this.rollback = {
        ...this.rollback,
        running: false,
        error: errorMessage(e),
      };
    }
  }

  @action askUninstall() {
    this.actionError = null;
    this.skipHooks = false;
    this.uninstalling = true;
  }

  @action cancelUninstall() {
    if (this.busy !== 'uninstall') this.uninstalling = false;
  }

  @action setSkipHooks(value) {
    this.skipHooks = value;
  }

  @action
  async uninstall() {
    this.busy = 'uninstall';
    this.actionError = null;
    try {
      await uninstallRelease(
        this.token,
        this.region,
        this.args.namespace,
        this.args.name,
        this.skipHooks
      );
      this.notifications.add({
        title: `Release ${this.args.name} uninstalled`,
        color: 'success',
      });
      this.uninstalling = false;
      this.router.transitionTo('packages.index');
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.busy = null;
    }
  }
}
