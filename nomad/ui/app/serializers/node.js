/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { inject as service } from '@ember/service';
import ApplicationSerializer from './application';

export default class NodeSerializer extends ApplicationSerializer {
  @service config;

  attrs = {
    isDraining: 'Drain',
    httpAddr: 'HTTPAddr',
    resources: 'NodeResources',
    reserved: 'ReservedResources',
  };

  mapToArray = ['Drivers', 'HostVolumes'];

  normalize(typeHash, hash) {
    // The Norca product version (the list stubs carry this one attribute);
    // Version is the Nomad API version the client is compatible with.
    hash.NorcaVersion = hash.Attributes?.['norca.version'] || '';
    return super.normalize(typeHash, hash);
  }

  extractRelationships(modelClass, hash) {
    const { modelName } = modelClass;
    const nodeURL = this.store
      .adapterFor(modelName)
      .buildURL(
        modelName,
        this.extractId(modelClass, hash),
        hash,
        'findRecord'
      );

    return {
      allocations: {
        links: {
          related: `${nodeURL}/allocations`,
        },
      },
    };
  }
}
