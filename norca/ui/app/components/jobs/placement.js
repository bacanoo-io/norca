/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { task } from 'ember-concurrency';

const q = (value) => JSON.stringify(String(value ?? ''));

function block(name, lines) {
  return `${name} {\n${lines
    .filter(Boolean)
    .map((l) => `  ${l}`)
    .join('\n')}\n}`;
}

function constraintText(c) {
  return block('constraint', [
    c.LTarget ? `attribute = ${q(c.LTarget)}` : null,
    c.Operand && c.Operand !== '=' ? `operator  = ${q(c.Operand)}` : null,
    c.RTarget ? `value     = ${q(c.RTarget)}` : null,
  ]);
}

function affinityText(a) {
  return block('affinity', [
    a.LTarget ? `attribute = ${q(a.LTarget)}` : null,
    a.Operand && a.Operand !== '=' ? `operator  = ${q(a.Operand)}` : null,
    a.RTarget ? `value     = ${q(a.RTarget)}` : null,
    `weight    = ${a.Weight ?? 50}`,
  ]);
}

function spreadText(s) {
  const targets = (s.SpreadTarget || []).map((t) =>
    block('target', [`value   = ${q(t.Value)}`, `percent = ${t.Percent}`])
      .split('\n')
      .join('\n  ')
  );
  return block('spread', [
    `attribute = ${q(s.Attribute)}`,
    `weight    = ${s.Weight ?? 50}`,
    ...targets,
  ]);
}

function allocAffinityText(kind, a) {
  const selector = Object.entries(a.Selector || {})
    .map(([k, v]) => `${k} = ${q(v)}`)
    .join(', ');
  return block(kind, [
    `selector { ${selector} }`,
    a.Namespace ? `namespace = ${q(a.Namespace)}` : null,
    a.Scope ? `scope     = ${q(a.Scope)}` : null,
    a.Required ? 'required  = true' : `weight    = ${a.Weight ?? 50}`,
  ]);
}

export function placementRules(spec) {
  if (!spec) return [];
  return [
    ...(spec.Constraints || []).map(constraintText),
    ...(spec.Affinities || []).map(affinityText),
    ...(spec.Spreads || []).map(spreadText),
    ...(spec.Colocate || []).map((a) => allocAffinityText('colocate', a)),
    ...(spec.Separate || []).map((a) => allocAffinityText('separate', a)),
  ];
}

/**
 * The Placement tab of the job page (UI plan, job details): the placement
 * rules of the job and its task groups (constraints, affinities, spreads,
 * and the colocate and separate rules between jobs), and the disruption
 * budget of each task group with its current state.
 */
export default class JobsPlacement extends Component {
  @service token;

  @tracked definition = null;
  @tracked disruption = {};
  @tracked loadError = null;

  constructor() {
    super(...arguments);
    this.load.perform();
  }

  get job() {
    return this.args.job;
  }

  get jobRules() {
    return placementRules(this.definition);
  }

  get jobRulesText() {
    return this.jobRules.join('\n\n');
  }

  get groups() {
    return (this.definition?.TaskGroups || []).map((tg) => {
      const budget = tg.Disruption || null;
      const status = this.disruption[tg.Name] || null;
      return {
        name: tg.Name,
        count: tg.Count,
        rules: [
          ...placementRules(tg),
          ...(tg.Tasks || []).flatMap((t) =>
            placementRules(t).map((r) => `# task ${t.Name}\n${r}`)
          ),
        ].join('\n\n'),
        budget,
        budgetText: budget
          ? budget.MinHealthy != null
            ? `min_healthy = ${budget.MinHealthy}`
            : `max_unavailable = ${budget.MaxUnavailable}`
          : null,
        status,
      };
    });
  }

  @task
  *load() {
    try {
      this.definition = yield this.job.fetchRawDefinition();
    } catch (e) {
      this.loadError = 'The job definition could not be loaded.';
      return;
    }
    if (!(this.definition?.TaskGroups || []).some((tg) => tg.Disruption)) {
      return;
    }
    try {
      const namespace = this.job.get('namespace.name') || 'default';
      const response = yield this.token.authorizedRequest(
        `/v1/job/${encodeURIComponent(
          this.job.plainId
        )}/disruption?namespace=${encodeURIComponent(namespace)}`
      );
      if (response.ok) this.disruption = (yield response.json()) || {};
    } catch (e) {
      // The rules still show without the current state.
    }
  }
}
