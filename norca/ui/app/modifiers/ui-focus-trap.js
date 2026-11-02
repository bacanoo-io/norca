/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { modifier } from 'ember-modifier';

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled]):not([type="hidden"])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(',');

export function focusableIn(element) {
  return [...element.querySelectorAll(FOCUSABLE)].filter(
    (el) => !el.closest('[inert]') && el.getClientRects().length > 0
  );
}

/**
 * Keeps keyboard focus inside the element while it is rendered (dialogs,
 * the command palette), calls `onEscape` on Escape and gives focus back to
 * the previously focused element when it is removed.
 *
 *   <div role="dialog" {{ui-focus-trap onEscape=this.close}}>...</div>
 *
 * The first `[autofocus]` or focusable descendant receives the focus
 * (`initialFocus=selector` overrides it).
 */
export default modifier((element, _positional, { onEscape, initialFocus }) => {
  const previous = document.activeElement;

  const target =
    (initialFocus && element.querySelector(initialFocus)) ||
    element.querySelector('[autofocus]') ||
    focusableIn(element)[0] ||
    element;
  if (target === element && !element.hasAttribute('tabindex')) {
    element.setAttribute('tabindex', '-1');
  }
  target.focus();

  const keydown = (event) => {
    if (event.key === 'Escape' && onEscape) {
      event.stopPropagation();
      onEscape(event);
      return;
    }
    if (event.key !== 'Tab') return;
    const items = focusableIn(element);
    if (!items.length) {
      event.preventDefault();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  element.addEventListener('keydown', keydown);
  return () => {
    element.removeEventListener('keydown', keydown);
    if (
      previous &&
      typeof previous.focus === 'function' &&
      previous.isConnected
    ) {
      previous.focus();
    }
  };
});
