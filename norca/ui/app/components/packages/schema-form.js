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
  applyJSON,
  parseDraft,
  setPath,
  validate,
  overrides,
  secrets,
  evaluateShowIf,
  runCommand,
  atomicDefault,
  compareVersionsDesc,
} from 'nomad-ui/utils/pack-form';

// The tabs of the form itself, after those of the pack.
const OWN_TABS = ['json', 'cli'];

/**
 * The backend of a pack of a cluster registry: what the form loads, plans
 * and applies. Another backend (utils/rigger: riggerBackend) has the same
 * members.
 */
function packBackend(form) {
  return {
    kind: 'pack',
    noun: 'pack',
    hasAtomic: true,
    hasDescription: true,
    hasAdopt: false,
    async load(region, version) {
      const r = await getCatalogPack(
        form.token,
        region,
        form.args.registry,
        form.args.pack,
        version
      );
      return { detail: r.body, version: r.body?.Pack?.Version || version };
    },
    schema(detail) {
      return detail?.UIFormSchema;
    },
    storedSecrets() {
      return [];
    },
    versions(detail) {
      return detail?.Versions || [];
    },
    compatibility(detail) {
      const e = detail?.Pack || {};
      if (!detail || e.Compatible !== false) return null;
      const missing = (e.MissingFeatures || []).join(', ');
      return missing || `requires Norca ${e.Requires?.Norca || ''}`.trim();
    },
    body({ version, values, secrets, description, atomic }) {
      const body = {
        Registry: form.args.registry,
        Pack: form.args.pack,
        Version: version,
        Values: values,
      };
      if (secrets.length) body.Secrets = secrets;
      if (description) body.Description = description;
      if (atomic !== undefined) body.Atomic = atomic;
      return body;
    },
    async plan(region, namespace, release, body) {
      const r = await planPackRelease(
        form.token,
        region,
        namespace,
        release,
        body
      );
      return {
        changes: r.body?.Changes || [],
        warnings: r.body?.Warnings || [],
      };
    },
    async apply(region, namespace, release, body) {
      const r = await deployPackRelease(
        form.token,
        region,
        namespace,
        release,
        body
      );
      return r.body;
    },
    command(state) {
      return runCommand({
        registry: form.args.registry,
        pack: form.args.pack,
        form: form.form,
        ...state,
      });
    },
  };
}

/**
 * The install (or upgrade) form of a pack of a cluster registry. The
 * servers synthesize the form schema from the variables of the pack
 * (GET /v1/pack/:registry/:pack), render the pack with the values and
 * apply it as a release revision (plan-pack, deploy-pack). With @backend
 * (utils/rigger: riggerBackend) it is the form of a module of Rigger.
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
  // Atomic mode of this install or upgrade: "default" leaves it to the
  // namespace (no Atomic in the request), "on" or "off" sets it.
  @tracked atomic = 'default';
  @tracked activeTab = null;

  @tracked values = {};
  @tracked redacted = [];
  @tracked drafts = {};
  // The text of the JSON tab while it is edited, and its error.
  @tracked jsonDraft = null;
  @tracked jsonError = null;
  @tracked draftErrors = {};
  @tracked errors = {};

  // Adopt takes over a release another manager applied or a stack owns
  // (a backend with hasAdopt).
  @tracked adopt = false;

  @tracked isPlanning = false;
  @tracked isDeploying = false;
  @tracked plan = null;
  @tracked actionError = null;

  backend = this.args.backend || packBackend(this);

  constructor() {
    super(...arguments);
    if (this.detail) this.reset(this.args.initialValues, { initial: true });
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
    return formSchema(this.backend.schema(this.detail));
  }

  // The variables a job reads from the secret store at run time: named,
  // not asked for.
  get storedSecrets() {
    return this.backend.storedSecrets(this.detail);
  }

  get versions() {
    const list = [...this.backend.versions(this.detail)];
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
      {
        id: 'json',
        label: 'JSON',
        icon: 'code',
        count: this.jsonError ? 1 : undefined,
      },
      { id: 'cli', label: 'CLI command', icon: 'terminal-2' },
    ];
  }

  // The overrides as JSON; the text being edited while it is edited.
  get jsonText() {
    return this.jsonDraft ?? JSON.stringify(this.payloadValues, null, 2);
  }

  @action setJson(text) {
    this.jsonDraft = text;
    const res = applyJSON(this.form, this.values, text);
    if (res.error) {
      this.jsonError = res.error;
      return;
    }
    this.jsonError = null;
    this.values = res.values;
    this.drafts = {};
    this.draftErrors = {};
    this.errors = {};
    this.plan = null;
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
      (OWN_TABS.includes(this.activeTab) ? null : this.form.tabs[0])
    );
  }

  get selectedTab() {
    return OWN_TABS.includes(this.activeTab)
      ? this.activeTab
      : this.currentTab?.id;
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

  // The atomic default of the namespace, null when its meta isn't known.
  get namespaceAtomic() {
    const ns = (this.system.namespaces || []).find(
      (n) => (n.get ? n.get('name') : n.Name || n) === this.namespace
    );
    return atomicDefault(ns?.get ? ns.get('meta') : ns?.Meta);
  }

  get atomicOptions() {
    const def = this.namespaceAtomic;
    return [
      {
        value: 'default',
        label:
          def === null
            ? 'Namespace default'
            : `Namespace default (${def ? 'on' : 'off'})`,
      },
      { value: 'on', label: 'On' },
      { value: 'off', label: 'Off' },
    ];
  }

  get atomicValue() {
    if (this.atomic === 'on') return true;
    if (this.atomic === 'off') return false;
    return undefined;
  }

  get atomicHelp() {
    return `A failed ${
      this.isUpgrade ? 'upgrade' : 'install'
    } restores the resources as they were before it, instead of leaving them as far as it got. On by default in namespaces with the meta environment = "production" or pack.atomic = "true".`;
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
    return this.backend.command({
      version: this.version,
      release: this.releaseName,
      namespace: this.namespace,
      atomic: this.atomicValue,
      values: this.payloadValues,
      secrets: this.payloadSecrets,
      adopt: this.adopt,
      form: this.form,
    });
  }

  get hasSecrets() {
    return this.form.fields.some((f) => f.secret);
  }

  get compatibilityProblem() {
    return this.backend.compatibility(this.detail);
  }

  get canSubmit() {
    return (
      Boolean(this.detail) && !this.isLoading && !this.compatibilityProblem
    );
  }

  // initial: from the constructor, which runs while the form renders: the
  // first tab is set without reading activeTab, as reading and then setting
  // it in the same render is an error.
  reset(initialValues, { initial = false } = {}) {
    const { values, redacted } = initialState(this.form, initialValues);
    this.values = values;
    this.redacted = redacted;
    this.drafts = {};
    this.draftErrors = {};
    this.errors = {};
    this.plan = null;
    this.actionError = null;
    if (initial || !this.form.tabs.some((t) => t.id === this.activeTab)) {
      this.activeTab = this.form.tabs[0]?.id || 'cli';
    }
  }

  fetchDetail(version) {
    return waitForPromise(this.backend.load(this.region, version));
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
      this.detail = r.detail;
      this.version = r.version;
      this.reset(keep);
    } catch (e) {
      this.loadError = errorMessage(e);
    } finally {
      this.isLoading = false;
    }
  }

  @action selectTab(id) {
    // Leaving the JSON tab shows the values as the form has them; a text
    // that doesn't apply stays until it is fixed.
    if (this.activeTab === 'json' && id !== 'json' && !this.jsonError) {
      this.jsonDraft = null;
    }
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

  @action setAtomic(value) {
    this.atomic = value;
    this.plan = null;
  }

  @action setDescription(value) {
    this.description = value;
  }

  @action setAdopt(value) {
    this.adopt = value;
    this.plan = null;
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
    if (field.widget === 'table') {
      // The errors of the cells of the table.
      const prefix = `${field.id}.`;
      if (Object.keys(this.errors).some((id) => id.startsWith(prefix))) {
        this.errors = Object.fromEntries(
          Object.entries(this.errors).filter(([id]) => !id.startsWith(prefix))
        );
      }
    }
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
    if (this.hasReleaseName && !this.releaseName.trim())
      errors.__name = 'The release name is required';
    if (this.jsonError) errors.__json = this.jsonError;
    this.errors = errors;
    const ids = Object.keys(errors);
    if (!ids.length) return true;
    const tab = this.form.tabs.find((t) => this.tabErrorCount(t));
    if (tab) this.activeTab = tab.id;
    else if (this.jsonError) this.activeTab = 'json';
    return false;
  }

  get requestBody() {
    return this.backend.body({
      version: this.version,
      release: this.releaseName.trim(),
      values: this.payloadValues,
      secrets: this.payloadSecrets,
      description: this.description.trim(),
      atomic: this.atomicValue,
      adopt: this.adopt,
      detail: this.detail,
    });
  }

  // A form of a release has its name; one of a stack has none of its own.
  get hasReleaseName() {
    return this.backend.hasReleaseName !== false;
  }

  get applyLabel() {
    if (this.backend.applyLabel) return this.backend.applyLabel;
    return this.isUpgrade ? 'Upgrade' : 'Install';
  }

  @action async runPlan() {
    this.actionError = null;
    this.plan = null;
    if (!this.check()) return;
    this.isPlanning = true;
    try {
      this.plan = await waitForPromise(
        this.backend.plan(
          this.region,
          this.namespace,
          this.releaseName.trim(),
          this.requestBody
        )
      );
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
      const body = await waitForPromise(
        this.backend.apply(
          this.region,
          this.namespace,
          this.releaseName.trim(),
          this.requestBody
        )
      );
      let title = this.isUpgrade
        ? `${this.releaseName} upgraded to revision ${body?.Revision}`
        : `${this.releaseName} installed`;
      if (body?.Action === 'unchanged') {
        title = `${this.releaseName} is unchanged (revision ${body.Revision})`;
      }
      if (body?.Operation) {
        title = `The apply of ${body.Stack || 'the stack'} runs as ${
          body.Operation
        }`;
      }
      this.notifications.add({ title, color: 'success' });
      this.args.onDeployed?.(body, {
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
