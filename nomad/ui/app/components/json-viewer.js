/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@ember/component';
import { classNames } from '@ember-decorators/component';
import classic from 'ember-classic-decorator';

@classic
@classNames('norca-json-viewer')
export default class JsonViewer extends Component {}
