/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';

const TONES = {
  successful: 'success',
  failed: 'danger',
  cancelled: 'warning',
  paused: 'warning',
  blocked: 'warning',
};

export default class JobsDeployment extends Component {
  @tracked isOpen = false;

  taskGroupColumns = [
    { key: 'name', label: 'Name' },
    { key: 'promotion', label: 'Needs promotion?' },
    { key: 'autoRevert', label: 'Auto revert?' },
    { key: 'canaries', label: 'Canaries' },
    { key: 'allocs', label: 'Allocations' },
    { key: 'healthy', label: 'Healthy' },
    { key: 'unhealthy', label: 'Unhealthy' },
    { key: 'deadline', label: 'Progress deadline' },
  ];

  get tone() {
    return TONES[this.args.deployment.status] || 'info';
  }

  @action
  toggle() {
    this.isOpen = !this.isOpen;
  }
}
