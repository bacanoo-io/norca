/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// ECharts (Apache-2.0) is loaded lazily with only the chart types and
// components the UI uses, so it stays out of the initial bundle.
let loading = null;

export function loadECharts() {
  if (!loading) {
    loading = Promise.all([
      import('echarts/core'),
      import('echarts/charts'),
      import('echarts/components'),
      import('echarts/renderers'),
    ]).then(([core, charts, components, renderers]) => {
      core.use([
        charts.BarChart,
        charts.LineChart,
        charts.PieChart,
        components.GridComponent,
        components.TooltipComponent,
        components.LegendComponent,
        components.DatasetComponent,
        renderers.SVGRenderer,
      ]);
      return core;
    });
    loading.catch(() => {
      loading = null;
    });
  }
  return loading;
}

function cssVar(element, name, fallback) {
  const value = getComputedStyle(element).getPropertyValue(name).trim();
  return value || fallback;
}

// Base options that align a chart with the design tokens of the current
// color scheme (read from the CSS custom properties at render time).
export function themeFor(element) {
  const text = cssVar(element, '--tblr-secondary', '#6c7a91');
  const border = cssVar(element, '--tblr-border-color', '#e6e7e9');
  const font = cssVar(element, '--tblr-font-sans-serif', 'sans-serif');
  return {
    backgroundColor: 'transparent',
    color: [
      cssVar(element, '--norca-chart-1', '#066fd1'),
      cssVar(element, '--norca-chart-2', '#2fb344'),
      cssVar(element, '--norca-chart-3', '#f76707'),
      cssVar(element, '--norca-chart-4', '#d63939'),
      cssVar(element, '--norca-chart-5', '#ae3ec9'),
      cssVar(element, '--norca-chart-6', '#0ca678'),
    ],
    textStyle: { color: text, fontFamily: font },
    tooltip: { trigger: 'item', confine: true },
    grid: { left: 8, right: 8, top: 16, bottom: 8, containLabel: true },
    animation: !window.matchMedia?.('(prefers-reduced-motion: reduce)').matches,
    axisPointer: { lineStyle: { color: border } },
  };
}
