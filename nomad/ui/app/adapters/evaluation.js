/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import ApplicationAdapter from './application';
import classic from 'ember-classic-decorator';

@classic
export default class EvaluationAdapter extends ApplicationAdapter {
  handleResponse(_status, headers) {
    const result = super.handleResponse(...arguments);
    result.meta = { nextToken: headers['x-nomad-nexttoken'] };
    return result;
  }

  urlForFindRecord(_id, _modelName, snapshot) {
    // Ember Data 4.12 asserts when the attributes of a record that is not
    // loaded yet are read; such a record is looked up in the default namespace.
    let namespace = 'default';
    try {
      namespace = snapshot.attr('namespace') || 'default';
    } catch (e) {
      namespace = 'default';
    }
    const baseURL = super.urlForFindRecord(...arguments);
    const url = `${baseURL}?namespace=${namespace}`;

    if (snapshot.adapterOptions?.related) {
      return `${url}&related=true`;
    }
    return url;
  }
}
