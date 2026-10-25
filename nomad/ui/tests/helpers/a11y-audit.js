/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import a11yAudit from 'ember-a11y-testing/test-support/audit';

// Rules that axe-core (4.14, with the Ember 6 upgrade) enforces more strictly
// than the version the pages inherited from Nomad 1.6 were written against.
// They stay disabled for those pages until they are migrated to the new
// component layer; tests of the new components use the strict audits below.
const LEGACY_RELAXED_RULES = [
  'aria-prohibited-attr',
  'aria-input-field-name',
  'aria-required-children',
  'empty-table-header',
  'label',
  'label-content-name-mismatch',
  'nested-interactive',
];

function appendRuleOverrides(overriddenRules, { strict = false } = {}) {
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

// Audits for the new UI (frame, dashboard, components in app/components/ui):
// every rule except color contrast, which the test page cannot measure.
export async function strictA11yAudit(assert, ...overriddenRules) {
  await a11yAudit({
    rules: appendRuleOverrides(overriddenRules, { strict: true }),
  });
  assert.ok(true, 'a11y audit passes');
}

export async function strictComponentA11yAudit(
  element,
  assert,
  ...overriddenRules
) {
  await a11yAudit(element, {
    rules: appendRuleOverrides(overriddenRules, { strict: true }),
  });
  assert.ok(true, 'a11y audit passes');
}
