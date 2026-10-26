/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import LocalList from 'nomad-ui/utils/local-list';

export default class AccessControlAuthMethodsController extends Controller {
  list = new LocalList({
    sortBy: 'name',
    searchProps: ['name'],
    filters: {
      type: (method, selection) => selection.includes(method.type),
    },
  });

  columns = [
    { key: 'name', label: 'Name', sortable: true, hideable: false },
    { key: 'type', label: 'Type', sortable: true },
    { key: 'default', label: 'Default', sortable: true },
    { key: 'tokenLocality', label: 'Token locality', sortable: true },
    { key: 'maxTokenTTL', label: 'Max token TTL', sortable: true },
    { key: 'createTime', label: 'Created', sortable: true },
  ];

  typeOptions = [
    { key: 'OIDC', label: 'OIDC' },
    { key: 'JWT', label: 'JWT' },
  ];

  get methods() {
    return (this.model || []).filter((method) => !method.isNew);
  }

  get rows() {
    return this.list.rows(this.methods);
  }
}
