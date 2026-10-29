/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { waitForPromise } from '@ember/test-waiters';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  getCatalogPack,
  planPackRelease,
  deployPackRelease,
} from 'nomad-ui/utils/releases';
import {
  formSchema,
  initialState,
  parseDraft,
  setPath,
  validate,
  overrides,
  secrets,
  evaluateShowIf,
  runCommand,
  compareVersionsDesc,
} from 'nomad-ui/utils/pack-form';

/**
 * The install (or upgrade) form of a pack of a cluster registry. The
 * servers synthesize the form schema from the variables of the pack
 * (GET /v1/pack/:registry/:pack), render the pack with the values and
 * apply it as a release revision (plan-pack, deploy-pack).
 *
 *   <Packages::SchemaForm @registry="builtin" @pack="redis" @version="1.0.0"
 *     @detail={{this.detail}} @region={{this.region}}
 *     @onDeployed={{this.deployed}} @onClose={{this.close}} />
 *
 * For an upgrade, @releaseName, @namespace and @initialValues (the values
 * of the deployed revision) are set and @isUpgrade is true.
 */
export default class PackagesSchemaForm extends Component {
  @service token;
  @service system;
  @service notifications;

  @tracked detail = this.args.detail || null;
  @tracked version = this.args.version || '';
  @tracked isLoading = false;
  @tracked loadError = null;
  @tracked versionNotice = null;

  @tracked releaseName = this.args.releaseName || this.defaultReleaseName;
  @tracked namespace = this.args.namespace || 'default';
  @tracked description = '';
  @tracked activeTab = null;

  @tracked values = {};
  @tracked redacted = [];
  @tracked drafts = {};
  @tracked draftErrors = {};
  @tracked errors = {};

  @tracked isPlanning = false;
  @tracked isDeploying = false;
  @tracked plan = null;
  @tracked actionError = null;

  constructor() {
    super(...arguments);
    if (this.detail) this.reset(this.args.initialValues);
    else this.load(this.version);
  }

  get defaultReleaseName() {
    return String(this.args.pack || 'release')
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, '-');
  }

  get isUpgrade() {
    return Boolean(this.args.isUpgrade);
  }

  get region() {
    return this.args.region || '';
  }

  get form() {
    return formSchema(this.detail?.UIFormSchema);
  }

  get entry() {
    return this.detail?.Pack || {};
  }

  get versions() {
    const list = [...(this.detail?.Versions || [])];
    if (this.version && !list.includes(this.version)) list.push(this.version);
    return [...new Set(list)].sort(compareVersionsDesc);
  }

  get tabs() {
    return [
      ...this.form.tabs.map((t) => ({
        id: t.id,
        label: t.label,
        icon: t.icon,
        count: this.tabErrorCount(t) || undefined,
      })),
      { id: 'cli', label: 'CLI command', icon: 'terminal-2' },
    ];
  }

  tabErrorCount(tab) {
    const ids = tab.groups.flatMap((g) => g.fields.map((f) => f.id));
    return Object.keys(this.errors).filter((id) =>
      ids.some((f) => id === f || id.startsWith(`${f}.`))
    ).length;
  }

  get currentTab() {
    return (
      this.form.tabs.find((t) => t.id === this.activeTab) ||
      (this.activeTab === 'cli' ? null : this.form.tabs[0])
    );
  }

  get selectedTab() {
    return this.activeTab === 'cli' ? 'cli' : this.currentTab?.id;
  }

  // The groups of the current tab with the fields x-ui-show-if leaves.
  get visibleGroups() {
    return (this.currentTab?.groups || [])
      .map((g) => ({
        name: g.name,
        fields: g.fields.filter((f) => evaluateShowIf(f.showIf, this.values)),
      }))
      .filter((g) => g.fields.length);
  }

  get namespaces() {
    const names = (this.system.namespaces || [])
      .map((ns) => (ns.get ? ns.get('name') : ns.Name || ns))
      .filter(Boolean);
    return [...new Set(['default', ...names])].sort();
  }

  get namespaceOptions() {
    const list = this.namespaces.includes(this.namespace)
      ? this.namespaces
      : [this.namespace, ...this.namespaces];
    return list;
  }

  get regions() {
    return (this.system.regions || []).slice().sort();
  }

  get payloadValues() {
    return overrides(this.form, this.values, this.args.initialValues);
  }

  get payloadSecrets() {
    return secrets(this.form, this.values);
  }

  get cliCommand() {
    return runCommand({
      registry: this.args.registry,
      pack: this.args.pack,
      version: this.version,
      release: this.releaseName,
      namespace: this.namespace,
      values: this.payloadValues,
      secrets: this.payloadSecrets,
      form: this.form,
    });
  }

  get hasSecrets() {
    return this.form.fields.some((f) => f.secret);
  }

  get compatibilityProblem() {
    const e = this.entry;
    if (!this.detail || e.Compatible !== false) return null;
    const missing = (e.MissingFeatures || []).join(', ');
    return missing || `requires Norca ${e.Requires?.Norca || ''}`.trim();
  }

  get canSubmit() {
    return (
      Boolean(this.detail) && !this.isLoading && !this.compatibilityProblem
    );
  }

  reset(initialValues) {
    const { values, redacted } = initialState(this.form, initialValues);
    this.values = values;
    this.redacted = redacted;
    this.drafts = {};
    this.draftErrors = {};
    this.errors = {};
    this.plan = null;
    this.actionError = null;
    if (!this.form.tabs.some((t) => t.id === this.activeTab)) {
      this.activeTab = this.form.tabs[0]?.id || 'cli';
    }
  }

  fetchDetail(version) {
    return waitForPromise(
      getCatalogPack(
        this.token,
        this.region,
        this.args.registry,
        this.args.pack,
        version
      )
    );
  }

  async load(version) {
    this.isLoading = true;
    this.loadError = null;
    this.versionNotice = null;
    try {
      let r;
      try {
        r = await this.fetchDetail(version);
      } catch (e) {
        // The version of a release may have left the registry: offer the
        // versions it has.
        if (!version || !this.isUpgrade || e?.status !== 404) throw e;
        r = await this.fetchDetail('');
        this.versionNotice = `Version ${version} is no longer in the ${this.args.registry} registry.`;
      }
      // Keep the values the user set that the new version still has.
      const keep = this.detail ? this.payloadValues : this.args.initialValues;
      this.detail = r.body;
      this.version = r.body?.Pack?.Version || version;
      this.reset(keep);
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action selectTab(id) {
    this.activeTab = id;
  }

  @action setVersion(version) {
    if (version !== this.version) this.load(version);
  }

  @action setReleaseName(value) {
    this.releaseName = value;
  }

  @action setNamespace(value) {
    this.namespace = value;
  }

  @action setDescription(value) {
    this.description = value;
  }

  clearError(id) {
    if (!(id in this.errors)) return;
    const errors = { ...this.errors };
    delete errors[id];
    this.errors = errors;
  }

  @action setValue(field, value) {
    this.values = setPath(this.values, field.path, value);
    this.redacted = this.redacted.filter(
      (k) => k !== field.key || value === ''
    );
    this.plan = null;
    this.clearError(field.id);
  }

  @action setDraft(field, text) {
    this.drafts = { ...this.drafts, [field.id]: text };
    const parsed = parseDraft(field, text);
    const draftErrors = { ...this.draftErrors };
    if (parsed.error) {
      draftErrors[field.id] = parsed.error;
      this.errors = { ...this.errors, [field.id]: parsed.error };
    } else {
      delete draftErrors[field.id];
      this.values = setPath(this.values, field.path, parsed.value);
      this.plan = null;
      this.clearError(field.id);
    }
    this.draftErrors = draftErrors;
  }

  // Validates; on errors opens the first tab that has one.
  check() {
    const errors = validate(this.form, this.values, {
      redacted: this.redacted,
      draftErrors: this.draftErrors,
      upgrade: this.isUpgrade,
    });
    if (!this.releaseName.trim())
      errors.__name = 'The release name is required';
    this.errors = errors;
    const ids = Object.keys(errors);
    if (!ids.length) return true;
    const tab = this.form.tabs.find((t) => this.tabErrorCount(t));
    if (tab) this.activeTab = tab.id;
    return false;
  }

  get requestBody() {
    const body = {
      Registry: this.args.registry,
      Pack: this.args.pack,
      Version: this.version,
      Values: this.payloadValues,
    };
    if (this.payloadSecrets.length) body.Secrets = this.payloadSecrets;
    if (this.description.trim()) body.Description = this.description.trim();
    return body;
  }

  @action async runPlan() {
    this.actionError = null;
    this.plan = null;
    if (!this.check()) return;
    this.isPlanning = true;
    try {
      const r = await planPackRelease(
        this.token,
        this.region,
        this.namespace,
        this.releaseName.trim(),
        this.requestBody
      );
      this.plan = {
        changes: r.body?.Changes || [],
        warnings: r.body?.Warnings || [],
      };
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isPlanning = false;
    }
  }

  @action async deploy() {
    this.actionError = null;
    if (!this.check()) return;
    this.isDeploying = true;
    try {
      const r = await deployPackRelease(
        this.token,
        this.region,
        this.namespace,
        this.releaseName.trim(),
        this.requestBody
      );
      this.notifications.add({
        title: this.isUpgrade
          ? `${this.releaseName} upgraded to revision ${r.body?.Revision}`
          : `${this.releaseName} installed`,
        color: 'success',
      });
      this.args.onDeployed?.(r.body, {
        name: this.releaseName.trim(),
        namespace: this.namespace,
        region: this.region,
      });
    } catch (e) {
      this.actionError = errorMessage(e);
    } finally {
      this.isDeploying = false;
    }
  }
}
