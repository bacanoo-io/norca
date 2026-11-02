/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

const { test, expect } = require('@playwright/test');

test('authenticated users can see their policies', async ({ page }) => {

  var NORCA_ADDR = process.env.NORCA_ADDR;
  if (NORCA_ADDR == undefined || NORCA_ADDR == "") {
    NORCA_ADDR = 'http://localhost:4646';
  }

  await page.goto(NORCA_ADDR+'/ui/settings/tokens');

  // smoke test that we reached the page
  const logo = page.locator('div.navbar-brand');
  await expect(logo).toBeVisible();


  const policies = page.locator('text=Policies')
  await expect(policies).toBeVisible();
});
