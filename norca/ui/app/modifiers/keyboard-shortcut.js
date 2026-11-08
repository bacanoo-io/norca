/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import Modifier from 'ember-modifier';
import { registerDestructor } from '@ember/destroyable';

export default class KeyboardShortcutModifier extends Modifier {
  @service keyboard;
  @service router;

  modify(
    element,
    _positional,
    {
      label,
      pattern = '',
      action = () => {},
      menuLevel = false,
      enumerated = false,
      exclusive = false,
    }
  ) {
    // Norca: a menu item without a shortcut registers nothing; it showed an
    // empty hint. Enumerated commands get their pattern from the service.
    if (!enumerated && !pattern?.length) return;

    let commands = [
      {
        label,
        action,
        pattern,
        element,
        menuLevel,
        enumerated,
        exclusive,
      },
    ];

    this.keyboard.addCommands(commands);
    registerDestructor(this, () => {
      this.keyboard.removeCommands(commands);
    });
  }
}
