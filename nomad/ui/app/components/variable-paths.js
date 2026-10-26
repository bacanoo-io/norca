/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check
import Component from '@glimmer/component';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import compactPath from '../utils/compact-path';
export default class VariablePathsComponent extends Component {
  @service router;
  @service can;

  /**
   * @returns {Array<Object.<string, Object>>}
   */
  get folders() {
    return Object.entries(this.args.branch.children).map(([name]) => {
      return compactPath(this.args.branch.children[name], name);
    });
  }

  get files() {
    return this.args.branch.files;
  }

  // The folders first, then the variables, as the rows of one table.
  get rows() {
    return [
      ...this.folders.map((folder) => ({ folder })),
      ...(this.files || []).map((file) => ({ file })),
    ];
  }

  @action
  handleRowClick(row) {
    if (row.folder) this.handleFolderClick(row.folder.data.absolutePath);
    else this.handleFileClick(row.file);
  }

  @action
  async handleFolderClick(path) {
    this.router.transitionTo('variables.path', path);
  }

  @action
  async handleFileClick({ path, variable: { id, namespace } }) {
    if (this.can.can('read variable', null, { path, namespace })) {
      this.router.transitionTo('variables.variable', id);
    }
  }
}
