/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, find, render, settled } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

function setEditorValue(selector, value) {
  find(`${selector} .CodeMirror`).CodeMirror.setValue(value);
  return settled();
}

module('Integration | Component | ui editor', function (hooks) {
  setupRenderingTest(hooks);

  test('JSON is checked against the schema before the review', async function (assert) {
    this.set('text', '{"Max": 3}');
    this.set('setText', (text) => this.set('text', text));
    this.set('schema', {
      type: 'object',
      required: ['Max'],
      properties: { Max: { type: 'integer', minimum: 0 } },
    });

    await render(hbs`
      <Ui::Editor
        @label="Scaling"
        @language="json"
        @schema={{this.schema}}
        @value={{this.text}}
        @onChange={{this.setText}}
        @original='{"Max": 3}'
        @onSubmit={{this.submit}}
      />
    `);

    assert.dom('[data-test-editor-problems]').doesNotExist();
    assert.dom('[data-test-editor-unchanged]').exists();

    await setEditorValue('[data-test-unified-editor]', '{"Max": -1}');
    assert.dom('[data-test-editor-problem]').hasText('Max: must be at least 0');
    assert.dom('[data-test-editor-review-button]').isDisabled();

    await setEditorValue('[data-test-unified-editor]', '{"Max": ');
    assert.dom('[data-test-editor-problem]').includesText('Invalid JSON');

    await setEditorValue('[data-test-unified-editor]', '{"Max": 5}');
    assert.dom('[data-test-editor-problems]').doesNotExist();
    assert.dom('[data-test-editor-review-button]').isNotDisabled();

    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('the review shows the diff and the submit applies the change', async function (assert) {
    this.set('text', 'a = 1\nb = 2\n');
    this.set('setText', (text) => this.set('text', text));
    this.set('submitted', null);
    this.set('submit', (text) => this.set('submitted', text));
    this.set('original', 'a = 1\nb = 2\n');

    await render(hbs`
      <Ui::Editor
        @label="Rules"
        @language="hcl"
        @value={{this.text}}
        @onChange={{this.setText}}
        @original={{this.original}}
        @onSubmit={{this.submit}}
        @submitLabel="Save rules"
      />
    `);

    await setEditorValue('[data-test-unified-editor]', 'a = 1\nb = 3\n');
    await click('[data-test-editor-review-button]');

    assert.dom('[data-test-editor-review]').exists();
    assert
      .dom('[data-test-text-diff-stats]')
      .hasText('1 line added, 1 line removed');
    assert.dom('[data-test-diff-line="add"]').includesText('b = 3');
    assert.dom('[data-test-diff-line="del"]').includesText('b = 2');

    await click('[data-test-editor-back]');
    assert.dom('[data-test-editor-review]').doesNotExist('back to editing');

    await click('[data-test-editor-review-button]');
    await click('[data-test-editor-submit]');
    assert.equal(this.submitted, 'a = 1\nb = 3\n');
    assert.dom('[data-test-editor-stage="edit"]').exists();

    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('remote validation problems are shown with their line', async function (assert) {
    this.set('text', 'job "x" {');
    this.set('setText', (text) => this.set('text', text));
    this.set('validate', async () => [{ line: 1, message: 'Unclosed block' }]);

    await render(hbs`
      <Ui::Editor @label="Job" @value={{this.text}} @onChange={{this.setText}} @validate={{this.validate}} />
    `);

    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-editor-problem]').hasText('Line 1: Unclosed block');
    assert.dom('[data-test-editor-review]').doesNotExist();
  });
});
