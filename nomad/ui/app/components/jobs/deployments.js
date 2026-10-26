/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import moment from 'moment';
import { sortRows } from 'nomad-ui/utils/sort-rows';

export default class JobsDeployments extends Component {
  get isMultiregion() {
    return (this.args.job.multiregionRegions || []).length > 0;
  }

  // Newest first, with the date shown when the day changes.
  get annotatedDeployments() {
    const deployments = sortRows(
      this.args.deployments || [],
      'versionSubmitTime',
      true
    );
    return deployments.map((deployment, index) => {
      let showDate = index === 0;
      if (!showDate) {
        const previous = deployments[index - 1].get('version.submitTime');
        const submitTime = deployment.get('version.submitTime');
        showDate =
          !!submitTime &&
          !!previous &&
          moment(previous)
            .startOf('day')
            .diff(moment(submitTime).startOf('day'), 'days') > 0;
      }
      return { deployment, showDate };
    });
  }
}
