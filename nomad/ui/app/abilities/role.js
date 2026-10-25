/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import classic from 'ember-classic-decorator';
import PolicyAbility from './policy';

/**
 * ACL roles bundle policies, so whoever may manage policies may manage
 * roles and no one else: the role ability is the policy ability.
 */
@classic
export default class RoleAbility extends PolicyAbility {}
