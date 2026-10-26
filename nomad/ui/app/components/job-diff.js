/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { equal } from '@ember/object/computed';
import Component from '@ember/component';
import { classNames, classNameBindings } from '@ember-decorators/component';
import classic from 'ember-classic-decorator';

@classic
@classNames('norca-job-diff')
@classNameBindings(
  'isEdited:norca-diff-edited',
  'isAdded:norca-diff-added',
  'isDeleted:norca-diff-deleted'
)
export default class JobDiff extends Component {
  diff = null;

  verbose = true;

  @equal('diff.Type', 'Edited') isEdited;
  @equal('diff.Type', 'Added') isAdded;
  @equal('diff.Type', 'Deleted') isDeleted;
}
