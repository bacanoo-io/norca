/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { guidFor } from '@ember/object/internals';
import { task } from 'ember-concurrency';
import Duration from 'duration-js';

const STORAGE_KEY = 'nomadDrainOptions';

const DURATIONS = [
  { label: '1 Hour', value: '1h' },
  { label: '4 Hours', value: '4h' },
  { label: '8 Hours', value: '8h' },
  { label: '12 Hours', value: '12h' },
  { label: '1 Day', value: '1d' },
  { label: 'Custom', value: 'custom' },
];

function readOptions() {
  try {
    return JSON.parse(window.localStorage.getItem(STORAGE_KEY)) || {};
  } catch (e) {
    return {};
  }
}

function writeOptions(options) {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(options));
  } catch (e) {
    // The options are a convenience.
  }
}

/**
 *   <Clients::DrainButton
 *     @client={{this.model}}
 *     @isDisabled={{cannot "write client"}}
 *     @onDrain={{this.drainNotify}}   (isUpdating)
 *     @onError={{this.setDrainError}} (error)
 *   />
 *
 * The last drain options are remembered (localStorage nomadDrainOptions).
 */
export default class ClientsDrainButton extends Component {
  durationQuickOptions = DURATIONS;
  formId = `drain-${guidFor(this)}`;

  @tracked isOpen = false;
  @tracked deadlineEnabled = false;
  @tracked selectedDuration = DURATIONS[0].value;
  @tracked customDuration = '';
  @tracked forceDrain = false;
  @tracked drainSystemJobs = true;
  @tracked parseError = '';

  get durationIsCustom() {
    return this.selectedDuration === 'custom';
  }

  get deadline() {
    if (!this.deadlineEnabled) return 0;
    if (this.durationIsCustom) return this.customDuration;
    return this.selectedDuration;
  }

  @action
  open() {
    if (this.args.isDisabled) return;
    const saved = readOptions();
    if ('deadlineEnabled' in saved)
      this.deadlineEnabled = saved.deadlineEnabled;
    if ('forceDrain' in saved) this.forceDrain = saved.forceDrain;
    if ('drainSystemJobs' in saved)
      this.drainSystemJobs = saved.drainSystemJobs;
    if (saved.selectedDurationQuickOption?.value) {
      this.selectedDuration = saved.selectedDurationQuickOption.value;
    }
    if ('customDuration' in saved)
      this.customDuration = saved.customDuration || '';
    this.parseError = '';
    this.isOpen = true;
  }

  @action
  close() {
    this.isOpen = false;
  }

  @action setDeadlineEnabled(value) {
    this.deadlineEnabled = value;
  }

  @action setDuration(value) {
    this.selectedDuration = value;
  }

  @action setCustomDuration(value) {
    this.parseError = '';
    this.customDuration = value;
  }

  @action setForceDrain(value) {
    this.forceDrain = value;
  }

  @action setDrainSystemJobs(value) {
    this.drainSystemJobs = value;
  }

  @action
  submit(event) {
    event?.preventDefault();
    this.drain.perform();
  }

  @task({ drop: true })
  *drain() {
    const client = this.args.client;
    if (!client) return;
    const isUpdating = client.isDraining;

    let deadline;
    try {
      deadline = new Duration(this.deadline).nanoseconds();
    } catch (err) {
      this.parseError = err.message;
      return;
    }

    const spec = {
      Deadline: deadline,
      IgnoreSystemJobs: !this.drainSystemJobs,
    };

    writeOptions({
      deadlineEnabled: this.deadlineEnabled,
      customDuration: this.customDuration,
      selectedDurationQuickOption: DURATIONS.find(
        (option) => option.value === this.selectedDuration
      ),
      drainSystemJobs: this.drainSystemJobs,
      forceDrain: this.forceDrain,
    });

    this.isOpen = false;

    try {
      if (this.forceDrain) {
        yield client.forceDrain(spec);
      } else {
        yield client.drain(spec);
      }
      this.args.onDrain?.(isUpdating);
    } catch (err) {
      this.args.onError?.(err);
    }
  }
}
