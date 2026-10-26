/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

import hbs from 'htmlbars-inline-precompile';
import { findAll, find, render } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import startMirage from 'ember-cli-mirage/start-mirage';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | jobs/children', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    this.store = this.owner.lookup('service:store');
    this.server = startMirage(this.owner);
    this.server.create('namespace');
    this.server.create('node-pool');
  });

  hooks.afterEach(function () {
    this.server.shutdown();
    window.localStorage.clear();
  });

  const props = (job, options = {}) =>
    Object.assign(
      {
        job,
        sortProperty: 'name',
        sortDescending: true,
        currentPage: 1,
      },
      options
    );

  test('lists each child', async function (assert) {
    this.server.create('job', 'periodic', {
      id: 'parent',
      childrenCount: 3,
      createAllocations: false,
    });

    await this.store.findAll('job');

    const parent = this.store.peekAll('job').findBy('plainId', 'parent');

    this.setProperties(props(parent));

    await render(hbs`
      <Jobs::Children
        @job={{this.job}}
        @sortBy={{this.sortProperty}}
        @sortDescending={{this.sortDescending}}
        @page={{this.currentPage}}
      />
    `);

    assert.equal(
      findAll('[data-test-job-name]').length,
      parent.get('children.length'),
      'A row for each child'
    );
  });

  test('eventually paginates', async function (assert) {
    assert.expect(5);

    const pageSize = 10;
    window.localStorage.nomadPageSize = pageSize;

    this.server.create('job', 'periodic', {
      id: 'parent',
      childrenCount: 11,
      createAllocations: false,
    });

    await this.store.findAll('job');

    const parent = this.store.peekAll('job').findBy('plainId', 'parent');

    this.setProperties(props(parent));

    await render(hbs`
      <Jobs::Children
        @job={{this.job}}
        @sortBy={{this.sortProperty}}
        @sortDescending={{this.sortDescending}}
        @page={{this.currentPage}}
      />
    `);

    const childrenCount = parent.get('children.length');
    assert.ok(
      childrenCount > pageSize,
      'Parent has more children than one page size'
    );
    assert.equal(
      findAll('[data-test-job-name]').length,
      pageSize,
      'Table length maxes out at 10'
    );
    assert.ok(find('[data-test-pagination-next]'), 'Next button is rendered');

    assert
      .dom('[data-test-pagination-info]')
      .includesText(
        '1–10 of 11',
        'Formats pagination to follow formula `startingIdx - endingIdx of totalTableCount'
      );

    await componentA11yAudit(this.element, assert);
  });

  test('is sorted based on the sortProperty and sortDescending properties', async function (assert) {
    assert.expect(6);

    this.server.create('job', 'periodic', {
      id: 'parent',
      childrenCount: 3,
      createAllocations: false,
    });

    await this.store.findAll('job');

    const parent = this.store.peekAll('job').findBy('plainId', 'parent');

    this.setProperties(props(parent));

    await render(hbs`
      <Jobs::Children
        @job={{this.job}}
        @sortBy={{this.sortProperty}}
        @sortDescending={{this.sortDescending}}
        @page={{this.currentPage}}
      />
    `);

    const sortedChildren = parent.get('children').sortBy('name');
    const childRows = findAll('[data-test-job-name]');

    sortedChildren.reverse().forEach((child, index) => {
      assert.equal(
        childRows[index].textContent.trim(),
        child.get('name'),
        `Child ${index} is ${child.get('name')}`
      );
    });

    await this.set('sortDescending', false);

    sortedChildren.forEach((child, index) => {
      assert.equal(
        childRows[index].textContent.trim(),
        child.get('name'),
        `Child ${index} is ${child.get('name')}`
      );
    });
  });
});
