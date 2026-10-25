/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { tracked } from '@glimmer/tracking';
import { interpret } from 'xstate';

// A minimal replacement of ember-statecharts (which does not support
// Ember 6): an XState interpreter whose current state is tracked. Like the
// original it starts lazily, on the first read of `state` or `send`.
class Statechart {
  @tracked _state;
  service = null;

  constructor(machine) {
    this.machine = machine;
  }

  _ensureStarted() {
    if (this.service) return;
    this.service = interpret(this.machine).onTransition((state) => {
      this._state = state;
    });
    this.service.start();
    this._state = this.service.state;
  }

  get state() {
    this._ensureStarted();
    return this._state;
  }

  send(...args) {
    this._ensureStarted();
    return this.service.send(...args);
  }

  stop() {
    this.service?.stop();
  }
}

// useMachine(machine).withConfig({ services, actions, guards })
export function useMachine(machine) {
  return {
    withConfig(config) {
      return new Statechart(machine.withConfig(config));
    },
  };
}

// @matchesState({ sidebar: 'open' }) isOpen; reads this.statechart.state
export function matchesState(stateValue, statechartProperty = 'statechart') {
  return function () {
    return {
      configurable: true,
      enumerable: true,
      get() {
        return !!this[statechartProperty]?.state?.matches(stateValue);
      },
    };
  };
}
