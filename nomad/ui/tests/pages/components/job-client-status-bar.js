/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { attribute, clickable, collection } from 'ember-cli-page-object';

// A distribution (Ui::Distribution): the segments of the bar and the
// legend.
export default (scope) => ({
  scope,

  slices: collection('[data-test-summary-slice]', {
    label: attribute('data-test-summary-slice'),
    click: clickable(),
  }),

  legend: {
    scope: '[data-test-legend]',

    items: collection('li', {
      label: attribute('data-test-legend-label'),
    }),

    clickableItems: collection('li[data-test-legend-clickable]', {
      label: attribute('data-test-legend-label'),
      click: clickable('a'),
    }),
  },

  visitSlice: async function (label) {
    await this.slices.toArray().findBy('label', label).click();
  },

  visitLegend: async function (label) {
    await this.legend.clickableItems.toArray().findBy('label', label).click();
  },
});
