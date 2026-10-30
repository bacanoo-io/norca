/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// The left menu, grouped per the information architecture of the UI plan.
// Only pages that exist are listed; the planned ones (packages, objects,
// deployments, services, network rules, node pools, quotas, scaling,
// admission policies, type definitions, events, backups) are added together
// with their features. `ability` hides an item the token cannot use;
// `shortcut` is the keyboard sequence (see the keyboard service).
export const NAVIGATION = [
  {
    group: 'Overview',
    items: [
      {
        id: 'dashboard',
        label: 'Dashboard',
        icon: 'layout-dashboard',
        route: 'index',
        shortcut: ['g', 'd'],
      },
    ],
  },
  {
    group: 'Workloads',
    items: [
      {
        id: 'jobs',
        label: 'Jobs',
        icon: 'briefcase',
        route: 'jobs',
        shortcut: ['g', 'j'],
      },
      {
        id: 'packages',
        label: 'Packages',
        icon: 'package',
        route: 'packages',
      },
      {
        id: 'objects',
        label: 'Objects',
        icon: 'box',
        route: 'objects',
      },
      {
        id: 'deployments',
        label: 'Deployments',
        icon: 'git-branch',
        route: 'deployments',
      },
    ],
  },
  {
    group: 'Services and networking',
    items: [
      {
        id: 'services',
        label: 'Services',
        icon: 'topology-star-3',
        route: 'services',
      },
      {
        id: 'network',
        label: 'Intentions and rules',
        icon: 'shield-lock',
        route: 'network',
      },
    ],
  },
  {
    group: 'Capacity',
    items: [
      {
        id: 'clients',
        label: 'Clients',
        icon: 'server-2',
        route: 'clients',
        shortcut: ['g', 'c'],
      },
      {
        id: 'topology',
        label: 'Topology',
        icon: 'hierarchy-3',
        route: 'topology',
        shortcut: ['g', 't'],
      },
      {
        id: 'storage',
        label: 'Storage',
        icon: 'database',
        route: 'csi',
        shortcut: ['g', 'r'],
      },
      {
        id: 'quotas',
        label: 'Quotas',
        icon: 'chart-pie',
        route: 'quotas',
        shortcut: ['g', 'q'],
        ability: 'read quota-spec',
      },
      {
        id: 'scaling',
        label: 'Scaling',
        icon: 'arrows-maximize',
        route: 'scaling',
      },
    ],
  },
  {
    group: 'Configuration',
    items: [
      {
        id: 'variables',
        label: 'Variables',
        icon: 'variable',
        route: 'variables',
        shortcut: ['g', 'v'],
        ability: 'list variables',
      },
      {
        id: 'secrets',
        label: 'Secrets',
        icon: 'lock',
        route: 'secrets',
        ability: 'list secret',
      },
      {
        id: 'admission',
        label: 'Admission policies',
        icon: 'shield-check',
        route: 'admission',
        ability: 'read admission',
      },
      {
        id: 'kinds',
        label: 'Type definitions',
        icon: 'list-details',
        route: 'kinds',
      },
    ],
  },
  {
    group: 'Operations',
    items: [
      {
        id: 'servers',
        label: 'Servers and regions',
        icon: 'server',
        route: 'servers',
        shortcut: ['g', 's'],
      },
      {
        id: 'evaluations',
        label: 'Evaluations',
        icon: 'list-details',
        route: 'evaluations',
        shortcut: ['g', 'e'],
      },
      {
        id: 'backups',
        label: 'Backups',
        icon: 'cloud-upload',
        route: 'backups',
        shortcut: ['g', 'b'],
        ability: 'read backup',
      },
    ],
  },
  {
    group: 'Access',
    items: [
      {
        id: 'access-control',
        label: 'Tokens, policies, roles',
        icon: 'key',
        route: 'access-control',
        shortcut: ['g', 'a'],
        ability: 'list policies',
      },
    ],
  },
];

export function navigationItems() {
  return NAVIGATION.flatMap(({ group, items }) =>
    items.map((item) => ({ ...item, group }))
  );
}
