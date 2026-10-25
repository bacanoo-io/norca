/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, render } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | ui basics', function (hooks) {
  setupRenderingTest(hooks);

  test('Ui::Icon renders a decorative Tabler icon, or a labelled one', async function (assert) {
    await render(
      hbs`<Ui::Icon @name="bell" /><Ui::Icon @name="search" @label="Search" @size="sm" />`
    );
    assert.dom('[data-test-icon="bell"]').hasAttribute('aria-hidden', 'true');
    assert
      .dom('[data-test-icon="bell"] path')
      .exists('the icon nodes are rendered');
    assert
      .dom('[data-test-icon="search"]')
      .hasAttribute('aria-label', 'Search');
    assert.dom('[data-test-icon="search"]').hasClass('icon-sm');
  });

  test('Ui::Button renders buttons and links with variants and icons', async function (assert) {
    this.clicked = 0;
    this.onClick = () => this.set('clicked', this.clicked + 1);
    await render(hbs`
      <Ui::Button @variant="primary" @icon="plus" data-test-a {{on "click" this.onClick}}>Run</Ui::Button>
      <Ui::Button @iconOnly={{true}} @icon="x" @label="Close" data-test-b />
      <Ui::Button @href="https://example.com" @external={{true}} data-test-c>Docs</Ui::Button>
      <Ui::Button @isLoading={{true}} data-test-d>Saving</Ui::Button>
    `);
    assert
      .dom('[data-test-a]')
      .hasClass('btn')
      .hasClass('btn-primary')
      .hasAttribute('type', 'button');
    assert.dom('[data-test-a] [data-test-icon="plus"]').exists();
    await click('[data-test-a]');
    assert.strictEqual(this.clicked, 1);
    assert
      .dom('[data-test-b]')
      .hasClass('btn-icon')
      .hasAttribute('aria-label', 'Close');
    assert
      .dom('[data-test-c]')
      .hasTagName('a')
      .hasAttribute('rel', 'noopener noreferrer');
    assert
      .dom('[data-test-d]')
      .isDisabled()
      .hasClass('btn-loading')
      .hasAttribute('aria-busy', 'true');
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('Ui::Badge and Ui::StatusIndicator map statuses to the status tones', async function (assert) {
    await render(hbs`
      <Ui::Badge @color="green" data-test-badge>running</Ui::Badge>
      <Ui::StatusIndicator @status="running" data-test-s1 />
      <Ui::StatusIndicator @status="failed" data-test-s2 />
      <Ui::StatusIndicator @status="draining" data-test-s3 />
      <Ui::StatusIndicator @status="something-else" data-test-s4 />
      <Ui::StatusIndicator @tone="danger" @label="3 failed" @dotOnly={{true}} data-test-s5 />
    `);
    assert.dom('[data-test-badge]').hasClass('bg-green-lt').hasText('running');
    assert.dom('[data-test-s1]').hasClass('status-green').hasText('running');
    assert.dom('[data-test-s2]').hasClass('status-red');
    assert.dom('[data-test-s3]').hasClass('status-orange');
    assert.dom('[data-test-s4]').hasClass('status-secondary');
    assert
      .dom('[data-test-s5]')
      .hasClass('status-dot')
      .hasAttribute('aria-label', '3 failed');
  });

  test('Ui::Card, Ui::PageHeader and Ui::EmptyState render their blocks', async function (assert) {
    await render(hbs`
      <Ui::PageHeader @pretitle="Overview" @title="Dashboard" @subtitle="All regions">
        <:actions><button type="button" data-test-action>Run</button></:actions>
      </Ui::PageHeader>
      <Ui::Card @title="Warnings" data-test-card>
        <:actions><span data-test-card-action>All</span></:actions>
        <:default>Body</:default>
        <:footer>Foot</:footer>
      </Ui::Card>
      <Ui::EmptyState @icon="briefcase" @title="No jobs yet" @message="Run a job to see it here.">
        <button type="button" data-test-empty-action>Run job</button>
      </Ui::EmptyState>
    `);
    assert.dom('[data-test-page-title]').hasText('Dashboard');
    assert.dom('[data-test-page-pretitle]').hasText('Overview');
    assert.dom('[data-test-page-subtitle]').hasText('All regions');
    assert.dom('[data-test-action]').exists();
    assert.dom('[data-test-card] [data-test-card-title]').hasText('Warnings');
    assert.dom('[data-test-card] .card-body').hasText('Body');
    assert.dom('[data-test-card] .card-footer').hasText('Foot');
    assert.dom('[data-test-card-action]').exists();
    assert.dom('[data-test-empty-state-title]').hasText('No jobs yet');
    assert
      .dom('[data-test-empty-state-message]')
      .hasText('Run a job to see it here.');
    assert.dom('[data-test-empty-action]').exists();
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('Ui::StackedBar describes its segments', async function (assert) {
    this.segments = [
      { label: 'running', value: 8, color: 'green' },
      { label: 'pending', value: 0, color: 'azure' },
      { label: 'failed', value: 2, color: 'red' },
    ];
    await render(
      hbs`<Ui::StackedBar @label="Allocations" @segments={{this.segments}} />`
    );
    assert
      .dom('[data-test-stacked-bar]')
      .hasAttribute(
        'aria-label',
        'Allocations: 8 running, 0 pending, 2 failed'
      );
    assert
      .dom('[data-test-stacked-bar-segment]')
      .exists({ count: 2 }, 'empty segments are not drawn');
    assert
      .dom('[data-test-stacked-bar-segment="running"]')
      .hasAttribute('style', /width: 80/)
      .hasClass('bg-green');
  });

  test('Ui::Steps, Ui::Timeline, Ui::Alert and Ui::Stat', async function (assert) {
    this.dismissed = false;
    this.dismiss = () => this.set('dismissed', true);
    this.items = [
      {
        icon: 'package',
        color: 'green',
        time: '14:10',
        title: 'Release 7',
        text: 'Upgraded',
      },
      { title: 'Something' },
    ];
    await render(hbs`
      <Ui::Steps @steps={{array "Choose" "Review" "Restore"}} @current={{1}} />
      <Ui::Timeline @items={{this.items}} />
      <Ui::Alert @tone="warning" @title="Old backup" @onDismiss={{this.dismiss}}>Three days old.</Ui::Alert>
      <Ui::Stat @id="x" @label="Blocked" @value={{2}} @hint="waiting" @color="orange" />
    `);
    assert
      .dom('[data-test-step="1"]')
      .hasClass('active')
      .hasAttribute('aria-current', 'step');
    assert.dom('[data-test-timeline-item]').exists({ count: 2 });
    assert.dom('[data-test-timeline-title]').hasText('Release 7');
    assert
      .dom('[data-test-alert="warning"] [data-test-alert-title]')
      .hasText('Old backup');
    await click('[data-test-alert-dismiss]');
    assert.true(this.dismissed);
    assert.dom('[data-test-stat="x"] [data-test-stat-value]').hasText('2');
    assert.dom('[data-test-stat="x"] [data-test-stat-hint]').hasText('waiting');
  });
});
