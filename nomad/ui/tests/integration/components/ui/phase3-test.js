/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, fillIn, render } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | ui phase 3 additions', function (hooks) {
  setupRenderingTest(hooks);

  test('Ui::Confirm asks for the typed value before it confirms', async function (assert) {
    this.confirmed = null;
    this.closed = false;
    this.confirm = (typed) => (this.confirmed = typed);
    this.close = () => (this.closed = true);
    await render(hbs`
      <Ui::Confirm @title="Delete the release?" @confirmLabel="Delete" @typedValue="web"
        @onConfirm={{this.confirm}} @onClose={{this.close}}>
        Its resources are removed.
      </Ui::Confirm>`);

    assert.dom('[data-test-modal-title]').hasText('Delete the release?');
    assert
      .dom('[data-test-confirm-body]')
      .hasText('Its resources are removed.');
    assert.dom('[data-test-confirm-submit]').isDisabled().hasText('Delete');
    await componentA11yAudit(this.element, assert, STRICT);

    await fillIn('[data-test-confirm-typed]', 'we');
    assert.dom('[data-test-confirm-submit]').isDisabled();
    await fillIn('[data-test-confirm-typed]', 'web');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(this.confirmed, 'web');

    await click('[data-test-confirm-cancel]');
    assert.true(this.closed);
  });

  test('Ui::Confirm without a typed value, with an error and while running', async function (assert) {
    this.running = false;
    this.calls = 0;
    this.confirm = () => this.calls++;
    await render(hbs`
      <Ui::Confirm @title="Roll back?" @tone="primary" @onConfirm={{this.confirm}}
        @isRunning={{this.running}} @error="409 another operation is running" />`);

    assert.dom('[data-test-confirm-typed]').doesNotExist();
    assert
      .dom('[data-test-confirm-error]')
      .hasText('409 another operation is running');
    assert.dom('[data-test-confirm-submit]').hasClass('btn-primary');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(this.calls, 1);

    this.set('running', true);
    assert.dom('[data-test-confirm-submit]').isDisabled();
    assert.dom('[data-test-confirm-cancel]').isDisabled();
  });

  test('Ui::Datagrid renders items and item blocks', async function (assert) {
    this.items = [
      { title: 'Engine', value: 'built-in' },
      { title: 'Raft index', value: 1834567, mono: true },
    ];
    await render(hbs`
      <Ui::Datagrid @items={{this.items}} as |grid|>
        <grid.Item @title="Status"><Ui::StatusIndicator @status="running" /></grid.Item>
      </Ui::Datagrid>`);

    assert.dom('[data-test-datagrid-item]').exists({ count: 3 });
    assert
      .dom('[data-test-datagrid-item="Engine"] [data-test-datagrid-value]')
      .hasText('built-in');
    assert
      .dom('[data-test-datagrid-item="Raft index"] [data-test-datagrid-value]')
      .hasClass('font-monospace');
    assert.dom('[data-test-datagrid-item="Status"]').includesText('running');
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('Ui::CodeBlock renders strings and objects and is focusable', async function (assert) {
    this.code = { spec: { replicas: 3 } };
    await render(hbs`<Ui::CodeBlock @label="Spec" @code={{this.code}} />`);

    assert.dom('[data-test-code-block]').hasAttribute('tabindex', '0');
    assert.dom('[data-test-code-block]').hasAttribute('aria-label', 'Spec');
    assert.strictEqual(
      this.element.querySelector('[data-test-code-block]').textContent,
      JSON.stringify(this.code, null, 2)
    );
    this.set('code', 'job.Type == "service"');
    assert.dom('[data-test-code-block]').hasText('job.Type == "service"');
    await componentA11yAudit(this.element, assert, STRICT);
  });
});
