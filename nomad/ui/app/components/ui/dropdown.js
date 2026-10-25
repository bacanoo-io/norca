/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { modifier } from 'ember-modifier';
import { createPopper } from '@popperjs/core';

/**
 * A dropdown menu: Tabler markup, positioned with Popper, behavior from
 * Ember (no Bootstrap JavaScript).
 *
 *   <Ui::Dropdown @label="Actions" @icon="dots-vertical" @iconOnly={{true}} as |dd|>
 *     <dd.Header>Job</dd.Header>
 *     <dd.Item @route="jobs.job.definition" @model={{@job}}>Definition</dd.Item>
 *     <dd.Divider />
 *     <dd.Item @tone="danger" {{on "click" this.stop}}>Stop</dd.Item>
 *   </Ui::Dropdown>
 *
 * The trigger can be replaced with the named block :trigger, which receives
 * `{ toggle, isOpen, id }`. Escape and a click outside close the menu, the
 * arrow keys move between the items, choosing an item closes it.
 *
 * @placement: a Popper placement (default bottom-end), @menuClass extra
 * classes of the menu (e.g. dropdown-menu-card).
 */
export default class UiDropdown extends Component {
  @tracked isOpen = false;
  id = `${guidFor(this)}-menu`;
  trigger = null;

  @action
  registerTrigger(element) {
    this.trigger = element;
  }

  @action
  toggle() {
    this.isOpen ? this.close() : this.open();
  }

  @action
  open() {
    this.isOpen = true;
    this.args.onOpen?.();
  }

  @action
  close({ restoreFocus = false } = {}) {
    if (!this.isOpen) return;
    this.isOpen = false;
    if (restoreFocus) this.trigger?.focus();
  }

  @action
  closeAndFocus() {
    this.close({ restoreFocus: true });
  }

  @action
  triggerKeydown(event) {
    if (event.key === 'ArrowDown' && !this.isOpen) {
      event.preventDefault();
      this.open();
    }
  }

  menu = modifier((element) => {
    const reference = this.trigger || element.parentElement;
    const popper = createPopper(reference, element, {
      placement: this.args.placement || 'bottom-end',
      modifiers: [{ name: 'offset', options: { offset: [0, 4] } }],
    });

    const items = () => [
      ...element.querySelectorAll(
        '.dropdown-item:not(.disabled):not([disabled])'
      ),
    ];

    const keydown = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        event.stopPropagation();
        this.closeAndFocus();
        return;
      }
      if (event.key === 'Tab') {
        this.close();
        return;
      }
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
      const list = items();
      if (!list.length) return;
      event.preventDefault();
      const current = list.indexOf(document.activeElement);
      let next = 0;
      if (event.key === 'ArrowDown') next = (current + 1) % list.length;
      if (event.key === 'ArrowUp')
        next = (current - 1 + list.length) % list.length;
      if (event.key === 'End') next = list.length - 1;
      list[next].focus();
    };

    const outside = (event) => {
      if (element.contains(event.target)) return;
      if (this.trigger?.contains(event.target)) return;
      this.close();
    };

    const chosen = (event) => {
      const item = event.target.closest('.dropdown-item');
      if (item && !item.hasAttribute('data-keep-open')) this.close();
    };

    // Move the focus into the menu when it was opened from the keyboard.
    if (document.activeElement === this.trigger) {
      const first = items()[0];
      if (first && this.args.focusFirst !== false) first.focus();
    }

    element.addEventListener('keydown', keydown);
    element.addEventListener('click', chosen);
    document.addEventListener('mousedown', outside, true);
    document.addEventListener('touchstart', outside, true);
    return () => {
      popper.destroy();
      element.removeEventListener('keydown', keydown);
      element.removeEventListener('click', chosen);
      document.removeEventListener('mousedown', outside, true);
      document.removeEventListener('touchstart', outside, true);
    };
  });
}
