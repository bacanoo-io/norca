/**
 * Copyright (c) HashiCorp, Inc.
 * Modifications Copyright (c) 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

const { chromium } = require('@playwright/test');

module.exports = async config => {

  var NORCA_TOKEN = process.env.NORCA_TOKEN;
  if (NORCA_TOKEN === undefined || NORCA_TOKEN === "") {
    return
  }

  var NORCA_ADDR = process.env.NORCA_ADDR;
  if (NORCA_ADDR == undefined || NORCA_ADDR == "") {
    NORCA_ADDR = 'http://localhost:4646';
  }

  const browser = await chromium.launch();
  const context = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await context.newPage();
  await page.goto(NORCA_ADDR+'/ui/settings/tokens');
  await page.fill('input[id="token-input"]', NORCA_TOKEN);
  await page.click('button:has-text("Set Token")', {strict: true});

  await page.context().storageState({ path: 'storageState.json' });
  await browser.close();
};
