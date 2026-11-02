/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';

const ICONS = {
  success: 'circle-check',
  critical: 'alert-circle',
  warning: 'alert-triangle',
};

export default class FrameToasts extends Component {
  @service notifications;

  iconFor(color) {
    return ICONS[color] || 'info-circle';
  }

  @action
  dismiss(close, flash) {
    close();
    flash.customCloseAction?.();
  }
}
