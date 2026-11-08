/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { render, click } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | backups/preview-objects', function (hooks) {
  setupRenderingTest(hooks);

  test('it shows every object, a page at a time', async function (assert) {
    assert.expect(6);
    this.preview = {
      Objects: Array.from({ length: 60 }, (_, i) => ({
        Type: 'quotas',
        Change: 'added',
        ID: `object-${i}`,
      })),
    };
    await render(hbs`<Backups::PreviewObjects @preview={{this.preview}} />`);

    assert.dom('[data-test-preview-objects] tbody tr').exists({ count: 25 });
    await componentA11yAudit(this.element, assert);
    assert.dom('[data-test-pagination-info]').includesText('1\u201325 of 60');

    await click('[data-test-pagination-next]');
    assert.dom('[data-test-pagination-info]').includesText('26\u201350 of 60');
    assert.dom('[data-test-preview-objects]').includesText('object-25');
    assert.dom('[data-test-preview-objects]').doesNotIncludeText('object-24');
  });

  test('it renders nothing without objects', async function (assert) {
    this.preview = { Objects: [] };
    await render(hbs`<Backups::PreviewObjects @preview={{this.preview}} />`);
    assert.dom('[data-test-preview-objects]').doesNotExist();
  });
});
