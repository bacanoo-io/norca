/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

const VARIANTS = {
  primary: 'btn-primary',
  secondary: '',
  danger: 'btn-danger',
  success: 'btn-success',
  warning: 'btn-warning',
  ghost: 'btn-ghost-secondary',
  link: 'btn-link',
  'outline-danger': 'btn-outline-danger',
};

const SIZES = { sm: 'btn-sm', md: '', lg: 'btn-lg' };

/**
 * A button, or a link styled as a button.
 *
 *   <Ui::Button @variant="primary" @icon="plus" {{on "click" this.run}}>Run job</Ui::Button>
 *   <Ui::Button @route="jobs.run" @icon="plus">Run job</Ui::Button>
 *   <Ui::Button @href="https://example.com" @icon="external-link">Docs</Ui::Button>
 *   <Ui::Button @icon="x" @iconOnly={{true}} @label="Close" />
 *
 * Arguments: @variant (primary, secondary, danger, success, warning, ghost,
 * link, outline-danger), @size (sm, md, lg), @icon, @iconOnly with @label,
 * @isLoading, @disabled, @type (button), @route/@model/@models/@query, @href.
 *
 * A route button does not look pressed while its route is active, and a
 * disabled route button is a disabled button (a link cannot be disabled).
 */
export default class UiButton extends Component {
  get classes() {
    const classes = ['btn', VARIANTS[this.args.variant || 'secondary'] || ''];
    classes.push(SIZES[this.args.size || 'md'] || '');
    if (this.args.iconOnly) classes.push('btn-icon');
    if (this.args.isLoading) classes.push('btn-loading');
    if (this.args.block) classes.push('w-100');
    return classes.filter(Boolean).join(' ');
  }

  get type() {
    return this.args.type || 'button';
  }

  get isDisabled() {
    return this.args.disabled || this.args.isLoading;
  }

  get models() {
    if (this.args.models) return this.args.models;
    if (this.args.model !== undefined) return [this.args.model];
    return [];
  }

  get query() {
    return this.args.query || {};
  }
}
