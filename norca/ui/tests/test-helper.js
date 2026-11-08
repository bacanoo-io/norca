/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

Error.stackTraceLimit = 60;
import Application from 'nomad-ui/app';
import config from 'nomad-ui/config/environment';
import * as QUnit from 'qunit';
import { setApplication } from '@ember/test-helpers';
import { start } from 'ember-exam/test-support';
import { setup } from 'qunit-dom';
import './helpers/flash-message';

setApplication(Application.create(config.APP));

setup(QUnit.assert);

// Every test starts without a stored token or a sign-in redirect: a token
// left by an earlier test makes the UI ask the cluster with it, and an
// unknown token on a cluster with ACLs sends every visit to the sign-in
// page. The hooks of the test run after this and set what they need.
QUnit.testStart(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

start();
