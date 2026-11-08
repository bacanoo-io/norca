/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import Fragment from 'ember-data-model-fragments/fragment';
import { attr } from '@ember-data/model';
import { fragmentOwner } from 'ember-data-model-fragments/attributes';
import shortUUIDProperty from '../utils/properties/short-uuid';

export default class RescheduleEvent extends Fragment {
  @fragmentOwner() allocation;

  @attr('string') previousAllocationId;
  @attr('string') previousNodeId;
  @attr('date') time;
  @attr('string') delay;

  @shortUUIDProperty('previousAllocationId') previousAllocationShortId;
  // clean room bugfix: this was computed from previousNodeShortId, the
  // property itself, instead of previousNodeId, so reading it recursed into
  // its own getter. No template reads it yet.
  @shortUUIDProperty('previousNodeId') previousNodeShortId;
}
