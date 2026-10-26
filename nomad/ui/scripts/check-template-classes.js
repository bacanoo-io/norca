/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-env node */
'use strict';

// Checks the templates of the pages migrated to the new UI (UI plan,
// acceptance criteria): no Bulma classes and no raw Tabler component
// classes outside the component layer (app/components/ui), no HashiCorp
// Design System components and no flight icons.
//
// Templates may use the layout utilities (grid, spacing, display and flex,
// text color and size, widths) and the own `norca-` classes; every other
// class needs a component in app/components/ui.
//
// Run with `node scripts/check-template-classes.js` (part of `yarn lint`).
// Add the templates of a page to MIGRATED when it is migrated.

const fs = require('fs');
const path = require('path');

const root = path.join(__dirname, '..');

// Files or directories (relative to app/) of the migrated pages. The
// global frame (components/frame) is built on Tabler's layout directly,
// like the component layer.
const MIGRATED = [
  'templates/index.hbs',
  'components/dashboard.hbs',
  // job page (phase 2)
  'templates/jobs/job.hbs',
  'templates/jobs/job',
  'templates/jobs/loading.hbs',
  'templates/jobs/run/index.hbs',
  'components/jobs',
  'components/allocations',
  'components/job-status',
  'components/job-editor',
  'components/job-editor.hbs',
  'components/job-diff.hbs',
  'components/job-diff-fields-and-objects.hbs',
  'components/job-dispatch.hbs',
  'components/placement-failure.hbs',
  'components/lifecycle-chart.hbs',
  'components/lifecycle-chart-row.hbs',
  'components/task-log.hbs',
  'components/exec/open-button.hbs',
  'components/variable-paths.hbs',
  'components/editable-variable-link.hbs',
  // allocation and task pages (phase 2)
  'templates/allocations.hbs',
  'templates/allocations',
  'components/fs',
  'components/primary-metric',
  'components/image-file.hbs',
  // jobs list, clients, Variables and access (phase 2)
  'templates/jobs/index.hbs',
  'templates/loading.hbs',
  'templates/clients.hbs',
  'templates/clients',
  'components/clients',
  'components/agent-monitor.hbs',
  'components/forbidden-message.hbs',
  'templates/variables.hbs',
  'templates/variables',
  'components/variable-form.hbs',
  'components/variable-form',
  'templates/access-control.hbs',
  'templates/access-control',
  'components/access',
  'components/token-editor.hbs',
  'components/role-editor.hbs',
  'components/policy-editor.hbs',
  // servers and regions, topology, storage, evaluations, keyboard shortcuts
  // (phase 4)
  'templates/servers.hbs',
  'templates/servers',
  'components/servers',
  'templates/topology.hbs',
  'components/topology',
  'components/topo-viz.hbs',
  'components/topo-viz',
  'templates/csi.hbs',
  'templates/csi',
  'components/storage',
  'templates/evaluations.hbs',
  'templates/evaluations',
  'components/evaluation-sidebar',
  'components/keyboard-shortcuts-modal.hbs',
  'templates/oidc-mock.hbs',
  'templates/settings.hbs',
  'templates/settings',
];

const ALLOWED = [
  /^norca-[a-z0-9-]+$/,
  // grid
  /^row$/,
  /^row-(cards|deck)$/,
  /^g[xy]?-\d$/,
  /^col(-(sm|md|lg|xl|xxl))?(-(\d{1,2}|auto))?$/,
  // spacing
  /^[mp][tbsexy]?-(sm-|md-|lg-|xl-)?(n?\d|auto)$/,
  // display, flex, alignment
  /^d-(sm-|md-|lg-|xl-)?(none|inline|inline-block|block|flex|inline-flex|grid|table-cell)$/,
  /^flex-(fill|grow-[01]|shrink-[01]|wrap|nowrap|column|row|column-reverse|row-reverse)$/,
  /^(sm|md|lg)-flex-/,
  /^align-(items|self|content)-(start|end|center|baseline|stretch)$/,
  /^align-(top|middle|bottom|baseline|text-top|text-bottom)$/,
  /^justify-content-(start|end|center|between|around|evenly)$/,
  /^gap-\d$/,
  /^order-(first|last|\d)$/,
  /^(ms|me)-auto$/,
  // typography
  /^h[1-6]$/,
  /^display-\d$/,
  // text
  /^text-(secondary|muted|danger|success|warning|info|primary|reset|body|start|end|center|truncate|nowrap|wrap|break|uppercase|lowercase|capitalize|decoration-none)$/,
  /^fw-(bold|bolder|semibold|medium|normal|light|lighter)$/,
  /^fs-\d$/,
  /^lh-(1|sm|base|lg)$/,
  /^small$/,
  /^font-monospace$/,
  /^tabular-nums$/,
  /^visually-hidden(-focusable)?$/,
  // sizing, position, misc
  /^w-(\d+|auto)$/,
  /^h-(\d+|auto)$/,
  /^mw-100$/,
  /^mh-100$/,
  /^min-w-0$/,
  /^overflow-(auto|hidden|visible)$/,
  /^position-(relative|absolute|sticky|static)$/,
  /^float-(start|end|none)$/,
  /^user-select-(all|auto|none)$/,
  /^opacity-\d+$/,
  /^cursor-pointer$/,
  /^border(-(top|bottom|start|end|0))?$/,
  /^rounded(-\d)?$/,
  /^list-unstyled$/,
  /^d-print-none$/,
];

const FORBIDDEN_COMPONENTS = [
  /<Hds::/,
  /<FlightIcon\b/,
  /\{\{\s*x-icon\b/,
  /\(\s*x-icon\b/,
  /\{\{\s*flight-icon\b/,
];

function walk(target, out = []) {
  const full = path.join(root, 'app', target);
  if (!fs.existsSync(full)) {
    throw new Error(`MIGRATED entry does not exist: app/${target}`);
  }
  if (fs.statSync(full).isDirectory()) {
    for (const entry of fs.readdirSync(full)) {
      walk(path.join(target, entry), out);
    }
  } else if (target.endsWith('.hbs')) {
    out.push(target);
  }
  return out;
}

// The string literals of a mustache that end up in the class: the branches
// of if/unless and the parts of concat. Literals compared with eq or passed
// to get are not classes.
function classLiterals(mustache) {
  const literals = [];
  const branch =
    /\((?:if|unless)\s+(?:\([^()]*\)|[^\s()]+)\s+((?:(["'])[\s\S]*?\2|\([^()]*\)|[^\s()]+)(?:\s+(?:(["'])[\s\S]*?\3|\([^()]*\)|[^\s()]+))?)\s*\)|\{\{\s*(?:if|unless)\s+(?:\([^()]*\)|[^\s()]+)\s+([\s\S]*?)\}\}/g;
  let m;
  while ((m = branch.exec(mustache))) {
    const args = m[1] || m[4] || '';
    args.replace(/(["'])([\s\S]*?)\1/g, (_, _q, str) => literals.push(str));
  }
  const concat = /\(concat\s+([\s\S]*?)\)|\{\{\s*concat\s+([\s\S]*?)\}\}/g;
  while ((m = concat.exec(mustache))) {
    (m[1] || m[2] || '').replace(/(["'])([\s\S]*?)\1/g, (_, _q, str) =>
      literals.push(str)
    );
  }
  return literals;
}

// The class tokens of a class attribute value: literal text outside
// mustaches, and the class literals inside them.
function classTokens(value) {
  const literals = [];
  const outside = value.replace(/\{\{[\s\S]*?\}\}/g, (mustache) => {
    literals.push(...classLiterals(mustache));
    return ' ';
  });
  const tokens = [];
  for (const text of [outside, ...literals]) {
    for (const token of text.split(/\s+/)) {
      if (token) tokens.push(token);
    }
  }
  return tokens;
}

// Class values of a template: class="..." attributes (quotes inside
// mustaches allowed) and class=/@class= values given as one mustache.
function classValues(source) {
  const values = [];
  const start = /(?:^|[\s(])@?class=/g;
  let m;
  while ((m = start.exec(source))) {
    let i = m.index + m[0].length;
    const line = source.slice(0, m.index).split('\n').length;
    const quote = source[i];
    let value = '';
    if (quote === '"' || quote === "'") {
      i++;
      let depth = 0;
      while (i < source.length) {
        if (source.startsWith('{{', i)) {
          depth++;
          value += '{{';
          i += 2;
          continue;
        }
        if (source.startsWith('}}', i) && depth > 0) {
          depth--;
          value += '}}';
          i += 2;
          continue;
        }
        if (source[i] === quote && depth === 0) break;
        value += source[i];
        i++;
      }
    } else if (source.startsWith('{{', i)) {
      const close = source.indexOf('}}', i);
      value = source.slice(i, close + 2);
    } else {
      continue;
    }
    values.push({ value, line });
  }
  return values;
}

function check() {
  const problems = [];
  const files = MIGRATED.flatMap((entry) => walk(entry));
  for (const file of files) {
    if (file.startsWith(path.join('components', 'ui'))) continue;
    const source = fs.readFileSync(path.join(root, 'app', file), 'utf8');
    const display = `app/${file.split(path.sep).join('/')}`;
    // Comments do not count.
    const code = source.replace(
      /\{\{!--[\s\S]*?--\}\}|\{\{![\s\S]*?\}\}/g,
      (c) => c.replace(/[^\n]/g, ' ')
    );
    for (const pattern of FORBIDDEN_COMPONENTS) {
      const m = code.match(pattern);
      if (m) {
        const line = code.slice(0, m.index).split('\n').length;
        problems.push(`${display}:${line}: ${m[0].trim()} is not allowed`);
      }
    }
    for (const { value, line } of classValues(code)) {
      for (const token of classTokens(value)) {
        if (!ALLOWED.some((re) => re.test(token))) {
          problems.push(
            `${display}:${line}: class "${token}" belongs in app/components/ui`
          );
        }
      }
    }
  }
  return { files, problems };
}

const { files, problems } = check();
problems.forEach((p) => console.log(p));
if (problems.length) {
  console.error(
    `${problems.length} problems in ${files.length} migrated templates`
  );
  process.exit(1);
}
console.log(`${files.length} migrated templates checked`);
