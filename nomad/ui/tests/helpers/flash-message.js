/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import { disableTimeout } from 'ember-cli-flash/test-support';

// Flash messages stay until dismissed in tests (their timers would keep
// settled() waiting and remove the messages before the assertions).
disableTimeout();
