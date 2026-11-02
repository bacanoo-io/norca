/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { modifier } from 'ember-modifier';
import { htmlSafe } from '@ember/template';
import { loadECharts, themeFor } from 'nomad-ui/utils/charts';

/**
 * A chart rendered with Apache ECharts (Apache-2.0), loaded on demand.
 *
 *   <Ui::Chart @label="Allocations by status" @option={{this.chartOption}} @height={{220}} />
 *
 * @option is an ECharts option object; colors default to the design tokens
 * (light or dark). @label is the accessible name; pass a textual summary of
 * the data in the block for screen readers (rendered visually hidden).
 */
export default class UiChart extends Component {
  @tracked failed = false;

  get style() {
    return htmlSafe(`height: ${this.args.height || 240}px`);
  }

  instance = null;
  element = null;
  observer = null;

  // The chart is created once and updated in place when @option changes
  // (live series), so it does not flicker.
  chart = modifier((element, [option]) => {
    if (this.element === element) {
      // While ECharts loads, the latest @option is used when it is ready.
      this.instance?.setOption({ ...themeFor(element), ...option }, true);
      return;
    }
    this.dispose();
    this.element = element;
    loadECharts()
      .then((echarts) => {
        if (this.isDestroying || this.element !== element) return;
        this.instance = echarts.init(element, null, { renderer: 'svg' });
        this.instance.setOption(
          { ...themeFor(element), ...this.args.option },
          true
        );
        this.observer = new ResizeObserver(
          () => this.instance && this.instance.resize()
        );
        this.observer.observe(element);
      })
      .catch(() => {
        if (!this.isDestroying) this.failed = true;
      });
  });

  dispose() {
    this.observer?.disconnect();
    this.instance?.dispose();
    this.observer = null;
    this.instance = null;
    this.element = null;
  }

  willDestroy() {
    super.willDestroy(...arguments);
    this.dispose();
  }
}
