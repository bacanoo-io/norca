/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/**
 * The install form of a catalog pack. The servers synthesize a JSON Schema
 * from the variables of the pack with x-ui-* directives
 * (pack/sdk/pack/variables/schema.go): x-ui-tabs (objects of id, label,
 * icon and order, or names), and per property x-ui-tab, x-ui-group,
 * x-ui-order, x-ui-widget, x-ui-unit, x-ui-presets and x-ui-show-if. The
 * variables without a default are required.
 *
 * The form keeps the full values (the defaults and the changes) and sends
 * only the overrides: the values that differ from the defaults, the
 * required ones and those the release already had.
 */

import ICONS from 'nomad-ui/utils/icons/tabler';

// The value the servers store for secret-like variables.
export const REDACTED = '(redacted)';

const TAB_ICONS = {
  general: 'settings',
  resources: 'cpu',
  network: 'network',
  storage: 'database',
  secrets: 'key',
  dependencies: 'packages',
};

// Packs name icons; the UI has a subset of Tabler (scripts/generate-icons.js).
export function knownIcon(name) {
  return name && ICONS[name] ? name : null;
}

// Widgets edited as text and parsed (see parseDraft).
const TEXT_WIDGETS = new Set(['tags', 'key-value', 'json']);

export function humanize(key) {
  return String(key || '')
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/[_.-]+/g, ' ')
    .trim()
    .replace(/^\w/, (c) => c.toUpperCase());
}

function tabId(name) {
  return String(name || 'general')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '');
}

function widgetOf(prop) {
  const declared = prop['x-ui-widget'];
  if (prop.type === 'object' && prop.properties && !declared) return 'object';
  switch (declared) {
    case 'switch':
    case 'segmented':
    case 'select':
    case 'number':
    case 'secret':
    case 'code':
    case 'tags':
    case 'key-value':
    case 'namespace-select':
    case 'region-select':
      return declared;
    case 'datacenter-multiselect':
      return 'tags';
    case 'slider':
      // The ends of the slider are its minimum and maximum.
      return Number.isFinite(prop.minimum) &&
        Number.isFinite(prop.maximum) &&
        prop.minimum < prop.maximum
        ? 'slider'
        : 'number';
    case 'object-list':
      // A table with a column per attribute of the items; lists of items
      // without declared attributes stay JSON.
      return prop.items?.properties ? 'table' : 'json';
    case 'node-pool-select':
    case 'storage-volume-select':
      return 'text';
  }
  if (prop.enum?.length) return prop.enum.length <= 3 ? 'segmented' : 'select';
  if (prop.type === 'boolean') return 'switch';
  if (prop.type === 'integer' || prop.type === 'number') return 'number';
  if (prop.type === 'array') {
    const item = prop.items?.type;
    return !item || ['string', 'number', 'integer'].includes(item)
      ? 'tags'
      : 'json';
  }
  if (prop.type === 'object') {
    return prop.properties ? 'object' : 'key-value';
  }
  return 'text';
}

function emptyValue(field) {
  switch (field.widget) {
    case 'switch':
      return false;
    case 'tags':
      return [];
    case 'key-value':
      return {};
    case 'object':
      return Object.fromEntries(
        field.children.map((c) => [
          c.key,
          c.hasDefault ? c.default : emptyValue(c),
        ])
      );
    case 'json':
      return field.type === 'object' ? {} : [];
    case 'table':
      return [];
    case 'number':
    case 'slider':
      return null;
    default:
      return '';
  }
}

function fieldOf(key, prop, required, path) {
  const widget = widgetOf(prop);
  const field = {
    key,
    path,
    id: path.join('.'),
    title: prop.title || humanize(key),
    description: prop.description || '',
    type: prop.type || 'string',
    widget,
    enum: prop.enum || null,
    hasDefault: prop.default !== undefined,
    default: prop.default,
    minimum: prop.minimum,
    maximum: prop.maximum,
    minItems: prop.minItems,
    maxItems: prop.maxItems,
    unit: prop['x-ui-unit'] || '',
    presets: prop['x-ui-presets'] || null,
    step: prop['x-ui-step'] ?? (prop.type === 'integer' ? 1 : null),
    tab: tabId(prop['x-ui-tab']),
    group: prop['x-ui-group'] || '',
    order: Number(prop['x-ui-order'] ?? 1000),
    showIf: prop['x-ui-show-if'] || null,
    enumLabels: prop['x-ui-enum-labels'] || {},
    secret: widget === 'secret' || prop['x-ui-secret'] === true,
    // A secret written to an item of a Variable of the pack, never to the
    // release values (x-ui-secret-path, x-ui-secret-item).
    secretPath: prop['x-ui-secret-path'] || '',
    secretItem: prop['x-ui-secret-item'] || key,
    required,
    integer: prop.type === 'integer',
    itemsNumeric: ['number', 'integer'].includes(prop.items?.type),
    children: [],
    columns: [],
  };
  if (widget === 'table') {
    const items = prop.items || {};
    field.columns = Object.entries(items.properties || {})
      .map(([k, p]) => columnOf(k, p, (items.required || []).includes(k)))
      .sort(byOrder);
  }
  if (widget === 'object') {
    field.children = Object.entries(prop.properties || {})
      .map(([k, p]) =>
        fieldOf(k, p, (prop.required || []).includes(k), [...path, k])
      )
      .sort(byOrder);
  }
  return field;
}

// A column of a table: the attribute of the items, shown with a compact
// widget (a select for a list of choices, a number, a checkbox or text).
function columnOf(key, prop, required) {
  const column = fieldOf(key, prop, required, [key]);
  switch (column.widget) {
    case 'segmented':
    case 'select':
    case 'number':
    case 'switch':
      break;
    case 'slider':
      column.widget = 'number';
      break;
    default:
      column.widget = 'text';
  }
  // Objects and lists in a cell are entered as JSON text.
  column.json = ['object', 'array'].includes(column.type);
  return column;
}

/** A new item of a table: the attributes with a default, the others unset. */
export function newRow(field) {
  const row = {};
  for (const c of field.columns) {
    if (c.hasDefault) row[c.key] = c.default;
    else if (c.widget === 'switch') row[c.key] = false;
  }
  return row;
}

/**
 * The item with a new cell value. An empty value of an optional attribute
 * is removed, so the pack applies its default.
 */
export function setCell(field, row, column, value) {
  const out = { ...(row || {}) };
  let v = value;
  if (column.widget === 'number') v = v === '' || v === null ? null : Number(v);
  if (column.json && typeof v === 'string') v = parseMaybeJSON(v);
  if ((v === '' || v === null || v === undefined) && !column.required) {
    delete out[column.key];
  } else {
    out[column.key] = v;
  }
  return out;
}

function byOrder(a, b) {
  return a.order - b.order || a.key.localeCompare(b.key);
}

/**
 * The tabs, groups and fields of a form schema:
 *   { tabs: [{ id, label, icon, groups: [{ name, fields }] }], fields }
 */
export function formSchema(schema) {
  const props = schema?.properties || {};
  const required = new Set(schema?.required || []);
  const fields = Object.entries(props)
    .map(([key, prop]) => fieldOf(key, prop || {}, required.has(key), [key]))
    .sort(byOrder);

  const declared = (
    Array.isArray(schema?.['x-ui-tabs']) ? schema['x-ui-tabs'] : []
  )
    .map((t, i) =>
      typeof t === 'string'
        ? { id: tabId(t), label: t, order: i }
        : {
            id: tabId(t.id || t.name || t.label || t.title),
            label: t.label || t.title || t.name || t.id,
            icon: t.icon,
            order: t.order ?? i,
          }
    )
    .sort((a, b) => a.order - b.order);
  // Tabs that properties use but x-ui-tabs leaves out come last.
  for (const f of fields) {
    if (!declared.some((t) => t.id === f.tab)) {
      declared.push({ id: f.tab, label: humanize(f.tab), order: Infinity });
    }
  }

  const tabs = declared
    .map((t) => {
      const groups = [];
      for (const f of fields.filter((f) => f.tab === t.id)) {
        let g = groups.find((g) => g.name === f.group);
        if (!g) groups.push((g = { name: f.group, fields: [] }));
        g.fields.push(f);
      }
      return {
        id: t.id,
        label: t.label,
        icon: knownIcon(t.icon) || TAB_ICONS[t.id] || 'settings',
        groups,
      };
    })
    .filter((t) => t.groups.length);

  return { tabs, fields };
}

export function getPath(values, path) {
  return path.reduce((v, k) => (v == null ? undefined : v[k]), values);
}

export function setPath(values, path, value) {
  const [head, ...rest] = path;
  const out = { ...(values || {}) };
  out[head] = rest.length ? setPath(out[head], rest, value) : value;
  return out;
}

function parseMaybeJSON(v) {
  if (typeof v !== 'string') return v;
  try {
    return JSON.parse(v);
  } catch (e) {
    return v;
  }
}

// Values of CLI installs are strings (the -var text); bring them to the
// type of the field.
function coerce(field, v) {
  if (v === undefined || v === null) return v;
  switch (field.widget) {
    case 'number':
    case 'slider': {
      const n = Number(v);
      return v === '' || isNaN(n) ? v : n;
    }
    case 'switch':
      return v === true || v === 'true';
    case 'tags':
    case 'key-value':
    case 'json':
    case 'object':
    case 'table':
      return parseMaybeJSON(v);
    default:
      return typeof v === 'string' ? v : JSON.stringify(v);
  }
}

/**
 * The initial state: the values (defaults merged with the values of the
 * release) and the secret fields whose stored value is redacted, which
 * have to be entered again.
 */
export function initialState(form, initialValues = {}) {
  const values = {};
  const redacted = [];
  for (const f of form.fields) {
    values[f.key] = f.hasDefault ? coerce(f, f.default) : emptyValue(f);
  }
  for (const [key, v] of Object.entries(initialValues || {})) {
    const f = form.fields.find((f) => f.key === key);
    if (v === REDACTED) {
      // Variable secrets keep their stored item when left empty.
      if (!f?.secretPath) redacted.push(key);
      values[key] = '';
    } else {
      values[key] = f ? coerce(f, v) : v;
    }
  }
  return { values, redacted };
}

// The text a field edited as text shows for a value.
export function draftOf(field, value) {
  if (field.widget === 'tags') return (value || []).join(', ');
  if (field.widget === 'key-value') {
    return Object.entries(value || {})
      .map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`)
      .join('\n');
  }
  if (field.widget === 'json') return JSON.stringify(value ?? null, null, 2);
  return value ?? '';
}

export function isTextWidget(field) {
  return TEXT_WIDGETS.has(field.widget);
}

/** Parses the text of a field: { value } or { error }. */
export function parseDraft(field, text) {
  if (field.widget === 'tags') {
    const numeric = field.itemsNumeric;
    const items = text
      .split(/[,\n]/)
      .map((s) => s.trim())
      .filter(Boolean);
    return { value: numeric ? items.map(Number) : items };
  }
  if (field.widget === 'key-value') {
    const out = {};
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      const i = line.indexOf('=');
      if (i < 1) return { error: `"${line.trim()}" is not key=value` };
      out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
    }
    return { value: out };
  }
  try {
    return { value: JSON.parse(text) };
  } catch (e) {
    return { error: 'Not valid JSON' };
  }
}

export function evaluateShowIf(predicate, values) {
  if (!predicate) return true;
  let field, test;
  if (Array.isArray(predicate)) {
    const [op, f, target] = predicate;
    field = f;
    test = (v) => {
      switch (op) {
        case '==':
          return String(v ?? '') === String(target ?? '');
        case '!=':
          return String(v ?? '') !== String(target ?? '');
        case 'in':
          return (target || []).map(String).includes(String(v));
        case 'falsy':
          return !v;
        default:
          return Boolean(v);
      }
    };
  } else if (typeof predicate === 'object') {
    field = predicate.field;
    test = (v) => {
      if ('equals' in predicate)
        return String(v ?? '') === String(predicate.equals ?? '');
      if ('notEquals' in predicate)
        return String(v ?? '') !== String(predicate.notEquals ?? '');
      if ('in' in predicate)
        return (predicate.in || []).map(String).includes(String(v));
      return Boolean(v);
    };
  } else {
    return true;
  }
  return test(getPath(values, String(field).split('.')));
}

function isEmpty(v) {
  return v === undefined || v === null || v === '';
}

/** The id of the error of a cell of a table. */
export function cellId(field, index, column) {
  return `${field.id}.${index}.${column.key}`;
}

/** The errors of the visible fields by field id. */
export function validate(
  form,
  values,
  { redacted = [], draftErrors = {}, upgrade = false } = {}
) {
  const errors = { ...draftErrors };
  // The error of a value: required, a number in its range, or one of the
  // choices. id is where the error is reported.
  const scalar = (f, id, v) => {
    if (f.required && isEmpty(v) && !(f.secretPath && upgrade)) {
      errors[id] = redacted.includes(f.key)
        ? `${f.title} is stored redacted: enter it again`
        : `${f.title} is required`;
      return false;
    }
    if (isEmpty(v)) return false;
    if (f.widget === 'number' || f.widget === 'slider') {
      const n = Number(v);
      if (isNaN(n)) errors[id] = `${f.title} must be a number`;
      else if (f.integer && !Number.isInteger(n))
        errors[id] = `${f.title} must be a whole number`;
      else if (f.minimum !== undefined && n < f.minimum)
        errors[id] = `${f.title} must be at least ${f.minimum}`;
      else if (f.maximum !== undefined && n > f.maximum)
        errors[id] = `${f.title} must be at most ${f.maximum}`;
    } else if (f.enum && !f.enum.map(String).includes(String(v))) {
      errors[id] = `${f.title} must be one of ${f.enum.join(', ')}`;
    }
    return true;
  };
  const check = (f, v) => {
    if (errors[f.id]) return;
    if (f.widget === 'table') {
      const rows = Array.isArray(v) ? v : [];
      if (f.required && !rows.length) errors[f.id] = `${f.title} is required`;
      else if (f.minItems !== undefined && rows.length < f.minItems)
        errors[f.id] = `${f.title} needs at least ${f.minItems} items`;
      else if (f.maxItems !== undefined && rows.length > f.maxItems)
        errors[f.id] = `${f.title} has at most ${f.maxItems} items`;
      rows.forEach((row, i) => {
        for (const c of f.columns) {
          scalar(c, cellId(f, i, c), row?.[c.key]);
        }
      });
      return;
    }
    if (!scalar(f, f.id, v)) return;
    for (const c of f.children) check(c, v?.[c.key]);
  };
  for (const f of form.fields) {
    if (!evaluateShowIf(f.showIf, values)) continue;
    check(f, values[f.key]);
  }
  // A redacted secret left empty would replace the stored one with the
  // default of the pack.
  for (const key of redacted) {
    const f = form.fields.find((f) => f.key === key);
    if (f && !errors[f.id] && isEmpty(values[key])) {
      errors[f.id] = `${f.title} is stored redacted: enter it again`;
    }
  }
  return errors;
}

function same(a, b) {
  return JSON.stringify(a) === JSON.stringify(b);
}

/**
 * The values of the form for the JSON text of the overrides (the JSON tab):
 * the defaults, the values of the Variable secrets (which the JSON leaves
 * out), and the values of the text. Returns { values } or { error }.
 */
export function applyJSON(form, values, text) {
  let parsed;
  try {
    parsed = JSON.parse(text);
  } catch (e) {
    return { error: `Not valid JSON: ${e.message}` };
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { error: 'The values must be a JSON object of variable names' };
  }
  const byKey = new Map(form.fields.map((f) => [f.key, f]));
  const unknown = Object.keys(parsed).filter(
    (k) => !byKey.has(k) || byKey.get(k).secretPath
  );
  if (unknown.length) {
    return {
      error: `Not variables of the pack${
        unknown.some((k) => byKey.get(k)?.secretPath)
          ? ' (secrets are entered on their tab)'
          : ''
      }: ${unknown.join(', ')}`,
    };
  }
  const out = initialState(form, {}).values;
  for (const f of form.fields) {
    if (f.secretPath) out[f.key] = values[f.key];
  }
  for (const [k, v] of Object.entries(parsed)) {
    out[k] = coerce(byKey.get(k), v);
  }
  return { values: out };
}

/**
 * The values to send: those that differ from the defaults, the required
 * ones and those the release had, of the visible fields. Empty values of
 * optional fields are left to the pack.
 */
export function overrides(form, values, initialValues = {}) {
  const out = {};
  for (const f of form.fields) {
    if (f.secretPath || !evaluateShowIf(f.showIf, values)) continue;
    const v = values[f.key];
    if (isEmpty(v) && !f.required) continue;
    const def = f.hasDefault ? coerce(f, f.default) : emptyValue(f);
    if (f.required || f.key in (initialValues || {}) || !same(v, def)) {
      out[f.key] = v;
    }
  }
  return out;
}

/** The Variable secrets of the form: [{ Path, Items }] (namespace of the release). */
export function secrets(form, values) {
  const byPath = new Map();
  for (const f of form.fields) {
    const v = values[f.key];
    if (!f.secretPath || isEmpty(v) || !evaluateShowIf(f.showIf, values))
      continue;
    if (!byPath.has(f.secretPath))
      byPath.set(f.secretPath, { Path: f.secretPath, Items: {} });
    byPath.get(f.secretPath).Items[f.secretItem] = String(v);
  }
  return [...byPath.values()];
}

function shellQuote(value) {
  return `'${String(value).replace(/'/g, `'\\''`)}'`;
}

/**
 * The CLI command equivalent to the install. Secret values are left as
 * placeholders. The registry is that of the cluster: the CLI needs a local
 * registry of the same source (norca pack registry add).
 */
export function runCommand({
  registry,
  pack,
  version,
  release,
  namespace,
  atomic,
  values,
  secrets = [],
  form,
}) {
  const parts = [`norca pack run ${shellQuote(pack)}`];
  if (registry) parts.push(`--registry=${shellQuote(registry)}`);
  if (version) parts.push(`--ref=${shellQuote(version)}`);
  if (release) parts.push(`--name=${shellQuote(release)}`);
  if (namespace && namespace !== 'default')
    parts.push(`--namespace=${shellQuote(namespace)}`);
  if (atomic !== undefined) parts.push(`--atomic=${atomic}`);
  for (const key of Object.keys(values || {}).sort()) {
    const f = form?.fields.find((f) => f.key === key);
    const v = values[key];
    const text = f?.secret
      ? '<secret>'
      : typeof v === 'string'
      ? v
      : JSON.stringify(v);
    parts.push(`--var ${shellQuote(`${key}=${text}`)}`);
  }
  for (const s of secrets) {
    for (const item of Object.keys(s.Items).sort()) {
      parts.push(`--var-secret ${shellQuote(`${s.Path}:${item}=<secret>`)}`);
    }
  }
  return parts.join(' \\\n  ');
}

/**
 * The atomic default of releases in a namespace, as the servers decide it
 * (structs.ReleaseAtomicDefault): the meta pack.atomic, else on in
 * namespaces with the meta environment production or prod. Null when the
 * meta of the namespace is not known.
 */
export function atomicDefault(meta) {
  if (!meta || typeof meta !== 'object') return null;
  const flag = String(meta['pack.atomic'] ?? '').toLowerCase();
  if (['true', '1', 't'].includes(flag)) return true;
  if (['false', '0', 'f'].includes(flag)) return false;
  return ['production', 'prod'].includes(
    String(meta.environment || '').toLowerCase()
  );
}

function versionParts(v) {
  return String(v || '')
    .replace(/^v/, '')
    .split(/[.-]/)
    .map((p) => (/^\d+$/.test(p) ? Number(p) : p));
}

/** Compares versions numerically by parts, newest first. */
export function compareVersionsDesc(a, b) {
  const pa = versionParts(a);
  const pb = versionParts(b);
  for (let i = 0; i < Math.max(pa.length, pb.length); i++) {
    const x = pa[i] ?? 0;
    const y = pb[i] ?? 0;
    if (x === y) continue;
    if (typeof x === 'number' && typeof y === 'number') return y - x;
    return String(y).localeCompare(String(x));
  }
  return 0;
}
