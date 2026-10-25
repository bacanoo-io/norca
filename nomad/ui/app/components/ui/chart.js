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

  chart = modifier((element, [option]) => {
    let instance = null;
    let observer = null;
    let disposed = false;

    loadECharts()
      .then((echarts) => {
        if (disposed) return;
        instance = echarts.init(element, null, { renderer: 'svg' });
        instance.setOption({ ...themeFor(element), ...option }, true);
        observer = new ResizeObserver(() => instance && instance.resize());
        observer.observe(element);
      })
      .catch(() => {
        if (!disposed) this.failed = true;
      });

    return () => {
      disposed = true;
      observer?.disconnect();
      instance?.dispose();
    };
  });
}
