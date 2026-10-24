/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import AbstractAbility from './abstract';
import { alias } from '@ember/object/computed';
import classic from 'ember-classic-decorator';

// ACL roles can only be managed with a management token, the same as
// policies and tokens.
@classic
export default class Role extends AbstractAbility {
  @alias('selfTokenIsManagement') canRead;
  @alias('selfTokenIsManagement') canList;
  @alias('selfTokenIsManagement') canWrite;
  @alias('selfTokenIsManagement') canUpdate;
  @alias('selfTokenIsManagement') canDestroy;
}
