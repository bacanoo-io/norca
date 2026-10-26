/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import './utils/array-prototype-extensions';
import './deprecation-workflow';
import Application from '@ember/application';
import Resolver from 'ember-resolver';
import loadInitializers from 'ember-load-initializers';
import config from 'nomad-ui/config/environment';
import { extendResolver } from 'ember-can';

export default class App extends Application {
  modulePrefix = config.modulePrefix;
  podModulePrefix = config.podModulePrefix;
  Resolver = extendResolver(Resolver);
}

loadInitializers(App, config.modulePrefix);
