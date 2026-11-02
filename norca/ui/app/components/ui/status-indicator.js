/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

// Status words of the Nomad API mapped to the status tokens. Status colors
// are used for status only (UI plan, visual direction).
const STATUS_TONES = {
  success: [
    'running',
    'ready',
    'healthy',
    'passing',
    'successful',
    'complete',
    'alive',
    'eligible',
    'up',
    'ok',
  ],
  info: ['pending', 'starting', 'deploying', 'initializing', 'queued'],
  warning: [
    'warning',
    'degraded',
    'draining',
    'disconnected',
    'paused',
    'blocked',
    'unknown',
    'ineligible',
    'left',
  ],
  danger: ['failed', 'dead', 'down', 'critical', 'lost', 'error', 'cancelled'],
};

const TONE_COLORS = {
  success: 'green',
  info: 'blue',
  warning: 'orange',
  danger: 'red',
  neutral: 'secondary',
};

export function toneFor(status) {
  const value = String(status || '').toLowerCase();
  for (const [tone, words] of Object.entries(STATUS_TONES)) {
    if (words.includes(value)) return tone;
  }
  return 'neutral';
}

/**
 * A status dot with an optional label.
 *
 *   <Ui::StatusIndicator @status="running" />            dot + "running"
 *   <Ui::StatusIndicator @tone="danger" @label="3 failed" />
 *   <Ui::StatusIndicator @status="pending" @animated={{true}} @dotOnly={{true}} />
 *
 * @tone overrides the tone derived from @status (success, info, warning,
 * danger, neutral). With @dotOnly the label becomes the accessible name.
 */
export default class UiStatusIndicator extends Component {
  get tone() {
    return this.args.tone || toneFor(this.args.status);
  }

  get color() {
    return TONE_COLORS[this.tone] || TONE_COLORS.neutral;
  }

  get label() {
    return this.args.label ?? this.args.status;
  }
}
