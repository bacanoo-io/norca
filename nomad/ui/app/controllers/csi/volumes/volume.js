/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Controller from '@ember/controller';
import { inject as service } from '@ember/service';

export default class VolumeController extends Controller {
  // Used in the template
  @service system;

  queryParams = [
    {
      volumeNamespace: 'namespace',
    },
  ];
  volumeNamespace = 'default';

  get volume() {
    return this.model;
  }

  // The position in the page header (Ui::Breadcrumb items).
  get breadcrumbs() {
    const volume = this.volume;
    const namespace = volume.get('namespace.name') || 'default';
    return [
      { label: 'Storage', route: 'csi.volumes.index', id: 'csi.index' },
      {
        label: 'Volumes',
        route: 'csi.volumes.index',
        query: { namespace },
        id: 'csi.volumes',
      },
      { label: volume.name, id: 'csi.volumes.volume' },
    ];
  }
}
