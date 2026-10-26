/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/**
 * Turns the breadcrumbs the controllers describe for the previous layout
 * ({ label, args: [route, ...models, queryParams?] }) into the items of
 * Ui::Breadcrumb ({ label, route, models, query, id }).
 */
export default function breadcrumbItems(crumbs = []) {
  return crumbs.map((crumb, index) => {
    const [route, ...rest] = crumb.args || [];
    const queryParams = rest.find((arg) => arg && arg.isQueryParams);
    const models = rest.filter((arg) => !(arg && arg.isQueryParams));
    return {
      label: crumb.label,
      route,
      models,
      query: queryParams ? queryParams.values : undefined,
      id: crumb.id || `${route}-${index}`,
    };
  });
}
