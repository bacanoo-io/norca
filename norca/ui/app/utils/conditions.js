/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// The tone of a condition of an object. Most conditions say something good
// when they are True (Ready, ProviderAvailable, Provisioned, NetworkCovered):
// False is the bad state. Some say something bad when they are True: a
// problem, or a step that is blocked. Some say that work is in progress:
// True is neither good nor bad.

// Conditions whose True is a problem, with the tone of their True.
const TRUE_IS_BAD = {
  Degraded: 'danger',
  JoinFailing: 'danger',
  ForeignNode: 'warning',
  ScaleDownBlocked: 'warning',
  DeletionBlocked: 'warning',
  RollBlocked: 'warning',
  RollBehind: 'warning',
  Drifted: 'warning',
};

// Conditions whose True is work in progress.
const TRUE_IS_WORK = new Set(['Progressing', 'Rolling']);

// conditionTone returns the tone of a condition of the type with the status
// ("True", "False" or "Unknown").
export function conditionTone(type, status) {
  const s = String(status);
  if (s !== 'True' && s !== 'False') return 'neutral';
  if (Object.hasOwn(TRUE_IS_BAD, type))
    return s === 'True' ? TRUE_IS_BAD[type] : 'success';
  if (TRUE_IS_WORK.has(type)) return s === 'True' ? 'info' : 'neutral';
  return s === 'True' ? 'success' : 'danger';
}

const rank = { danger: 3, warning: 2, info: 1, success: 0, neutral: 0 };

// worstCondition returns the type, the status and the tone of the condition
// of the list with the worst tone, danger before warning; null when none is
// a problem.
export function worstCondition(list) {
  let worst = null;
  for (const c of list || []) {
    const type = c.type || c.Type;
    const tone = conditionTone(type, c.status ?? c.Status);
    if (tone !== 'danger' && tone !== 'warning') continue;
    if (!worst || rank[tone] > rank[worst.tone])
      worst = { type, status: String(c.status ?? c.Status), tone };
  }
  return worst;
}
