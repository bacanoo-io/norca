/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import a11yAudit from 'ember-a11y-testing/test-support/audit';

// Rules that axe-core (4.14, with the Ember 6 upgrade) enforces more strictly
// than the version the pages inherited from Nomad 1.6 were written against.
// They stay disabled for those pages until they are migrated to the new
// component layer; tests of the new UI pass STRICT.
const LEGACY_RELAXED_RULES = [
  'aria-prohibited-attr',
  'aria-input-field-name',
  'aria-required-children',
  'empty-table-header',
  'label',
  'label-content-name-mismatch',
  'nested-interactive',
];

// Pass STRICT as one of the overridden rules to audit with every rule
// (tests of the new UI: frame, dashboard, app/components/ui).
export const STRICT = 'norca:strict';

function appendRuleOverrides(overriddenRules) {
  const strict = overriddenRules.includes(STRICT);
  overriddenRules = overriddenRules.filter((rule) => rule !== STRICT);
  const rules = {
    'color-contrast': {
      enabled: false,
    },
    'heading-order': {
      enabled: false,
    },
  };

  if (!strict) {
    LEGACY_RELAXED_RULES.forEach((rule) => (rules[rule] = { enabled: false }));
  }

  overriddenRules.forEach((rule) => (rules[rule] = { enabled: false }));

  return rules;
}

export default async function defaultA11yAudit(assert, ...overriddenRules) {
  await a11yAudit({ rules: appendRuleOverrides(overriddenRules) });
  assert.ok(true, 'a11y audit passes');
}

export async function componentA11yAudit(element, assert, ...overriddenRules) {
  await a11yAudit(element, { rules: appendRuleOverrides(overriddenRules) });
  assert.ok(true, 'a11y audit passes');
}
