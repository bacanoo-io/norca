/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

export default class ServersRegions extends Component {
  columns = [
    { key: 'region', label: 'Region' },
    { key: 'connection', label: 'Connection' },
    { key: 'leader', label: 'Leader' },
    { key: 'servers', label: 'Servers' },
    { key: 'raft', label: 'Raft peers' },
  ];
}
