/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import Backups from 'nomad-ui/tests/pages/backups';
import seedBackups from 'nomad-ui/mirage/norca/seeds/backups';
import { backupIdentity } from 'nomad-ui/mirage/norca/backups';
import { collection } from 'nomad-ui/mirage/norca/util';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

async function unlock() {
  await fillIn('[data-test-identity-input]', backupIdentity);
  await click('[data-test-identity-submit]');
}

module('Acceptance | backups', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    this.backupsService = this.owner.lookup('service:backups');
    this.backupsService.forgetIdentity();
  });

  test('the list shows the schedule, the destinations and the backups, newest first', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await Backups.visit();

    assert.strictEqual(currentURL(), '/backups');
    assert.dom('[data-test-page-title]').hasText('Backups');
    assert.dom('[data-test-page-pretitle]').hasText('Operations');
    assert.dom('[data-test-schedule-label]').hasText('every hour');
    assert.dom('[data-test-last-success]').exists();
    assert.strictEqual(Backups.rows.length, 4);
    assert.strictEqual(Backups.rows[0].trigger, 'Scheduled');
    assert.strictEqual(Backups.rows[3].status, 'failed');
    assert.dom('[data-test-gutter-link="backups"]').exists();
    assert.dom('[data-test-schedule-warning]').doesNotExist();
    await a11yAudit(assert, STRICT);
  });

  test('the schedule and the recent restores cards of a row have the same height', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await Backups.visit();

    const schedule = document.querySelector('[data-test-schedule]');
    const restores = document.querySelector('[data-test-recent-restores]');
    const a = schedule.getBoundingClientRect();
    const b = restores.getBoundingClientRect();
    // Side by side on wide screens (then of the same height), stacked on
    // narrow ones.
    const sameRow = Math.round(a.top) === Math.round(b.top);
    assert.ok(
      sameRow ? Math.round(a.height) === Math.round(b.height) : b.top > a.top,
      `schedule ${a.top}/${a.height}, restores ${b.top}/${b.height}`
    );
  });

  test('an overdue schedule becomes a warning that says what to do', async function (assert) {
    useToken(null, 'management');
    seedBackups(server, { overdue: true });
    await Backups.visit();

    assert.dom('[data-test-schedule-warning]').exists({ count: 1 });
    assert.dom('[data-test-schedule-warning]').includesText('overdue');
    assert.dom('[data-test-schedule-warning]').includesText('connection reset');
    assert
      .dom('[data-test-schedule-warning]')
      .includesText('Check the destinations');
  });

  test('the dashboard shows the last backup per region and warns when it is overdue', async function (assert) {
    useToken(null, 'management');
    seedBackups(server, { overdue: true });
    await visit('/');

    assert.dom('[data-test-region-backup]').includesText('ago');
    assert.dom('[data-test-region-backup]').includesText('overdue');
    assert
      .dom('[data-test-warnings]')
      .includesText(
        'The last successful backup of the local region is overdue'
      );
    await click('[data-test-region-backup] a');
    assert.strictEqual(currentURL(), '/backups');
  });

  test('without backups the empty state names the next scheduled run and offers a manual backup', async function (assert) {
    useToken(null, 'management');
    seedBackups(server, { empty: true });
    await Backups.visit();

    assert.ok(Backups.hasEmptyState);
    assert
      .dom('[data-test-backups-empty]')
      .includesText('The first scheduled backup runs');
    assert.dom('[data-test-backups-empty] button').hasText('Back up now');
  });

  test('back up now sends the note and the pin and the new backup appears', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await Backups.visit();
    await Backups.backupNow();
    assert.dom('[data-test-backup-now-modal]').exists();
    await a11yAudit(assert, STRICT);

    await fillIn('[data-test-backup-now-note]', 'before the upgrade');
    await click('[data-test-backup-now-pin] input');
    await click('[data-test-backup-now-submit]');

    const request = lastRequest('POST', '/v1/operator/backups');
    const body = JSON.parse(request.requestBody);
    assert.deepEqual(body.Labels, { note: 'before the upgrade' });
    assert.true(body.Pin);
    assert.true(body.Detach);
    assert.dom('[data-test-backup-now-modal]').doesNotExist();
    assert.strictEqual(Backups.rows.length, 5);
  });

  test('a backup can be pinned and unpinned from the list', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await Backups.visit();
    const id = server.db.norcaBackups[0].ID;

    await click(`[data-test-pin="${id}"]`);
    let body = JSON.parse(
      lastRequest('PUT', `/v1/operator/backup/${id}`).requestBody
    );
    assert.true(body.Pinned);
    assert.dom(`[data-test-pin="${id}"]`).hasAttribute('aria-pressed', 'true');

    await click(`[data-test-pin="${id}"]`);
    body = JSON.parse(
      lastRequest('PUT', `/v1/operator/backup/${id}`).requestBody
    );
    assert.false(body.Pinned);
  });

  test('a token with backup read cannot back up, pin or delete', async function (assert) {
    useToken({ Backup: { Policy: 'read' } });
    seedBackups(server);
    await Backups.visit();

    assert.dom('[data-test-backup-now]').doesNotExist();
    assert.dom('[data-test-pin]').doesNotExist();
    assert.strictEqual(Backups.rows.length, 4);

    await click(`[data-test-backup-link="${server.db.norcaBackups[0].ID}"]`);
    assert.dom('[data-test-delete-backup]').doesNotExist();
    assert.dom('[data-test-toggle-pin]').doesNotExist();
    assert.dom('[data-test-restore-link]').exists();
  });

  test('a token without backup permissions does not see the menu item', async function (assert) {
    useToken({
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    seedBackups(server);
    await visit('/');
    assert.dom('[data-test-gutter-link="backups"]').doesNotExist();
  });

  test('the details show the manifest, the destinations and the content counts', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const backup = server.db.norcaBackups[0];
    await Backups.visitBackup({ id: backup.ID });

    assert.dom('[data-test-page-title]').hasText(backup.ID);
    assert
      .dom('[data-test-backup-manifest]')
      .includesText(String(backup.RaftIndex));
    assert.dom('[data-test-destination-status="primary"]').hasText('verified');
    assert.dom('[data-test-summary-count="jobs"]').hasText('3');
    await a11yAudit(assert, STRICT);
  });

  test('deleting a pinned backup shows the reason; an unpinned one is deleted', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const pinned = server.db.norcaBackups.findBy({ Pinned: true });
    await Backups.visitBackup({ id: pinned.ID });
    await click('[data-test-delete-backup]');
    await a11yAudit(assert, STRICT);
    await click('[data-test-confirm-submit]');
    assert
      .dom('[data-test-confirm-error]')
      .includesText('is pinned; unpin it first');
    await click('[data-test-confirm-cancel]');

    const plain = server.db.norcaBackups[1];
    await Backups.visitBackup({ id: plain.ID });
    await click('[data-test-delete-backup]');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(currentURL(), '/backups');
    assert.notOk(server.db.norcaBackups.findBy({ ID: plain.ID }));
  });

  test('a download link is generated on request and expires', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await Backups.visitBackup({ id: server.db.norcaBackups[0].ID });
    await click('[data-test-download-link]');
    assert.dom('[data-test-download]').includesText('expires');
    assert.dom('[data-test-download-url]').hasValue(/X-Amz-Signature/);
  });

  test('browsing asks for the age identity, then lists the content by type', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const backup = server.db.norcaBackups[0];
    await visit(`/backups/${backup.ID}?tab=contents`);

    assert.ok(Backups.identity.isPresent);
    await a11yAudit(assert, STRICT);
    await unlock();

    const request = lastRequest('POST', `/backup/${backup.ID}/contents`);
    assert.strictEqual(
      JSON.parse(request.requestBody).AgeIdentity,
      backupIdentity
    );
    assert
      .dom('[data-test-contents-type="jobs"] [data-test-ui-table-row]')
      .exists({ count: 3 });

    await click('[data-test-job-versions="api"]');
    assert
      .dom('[data-test-contents-type="job-versions"] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert.dom('[data-test-job-filter]').includesText('Versions of api');

    await click('[data-test-clear-job]');
    await click('[data-test-tab="allocations"]');
    assert
      .dom('[data-test-contents-type="allocations"] [data-test-ui-table-row]')
      .exists({ count: 2 });

    await click('[data-test-tab="variables"]');
    assert
      .dom('[data-test-contents-type="variables"]')
      .includesText('db_password');
    assert
      .dom('[data-test-contents-type="variables"]')
      .includesText('values are never shown');
    await a11yAudit(assert, STRICT);
  });

  test('a wrong identity is rejected and asked again', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    await visit(`/backups/${server.db.norcaBackups[0].ID}?tab=contents`);
    await fillIn('[data-test-identity-input]', 'AGE-SECRET-KEY-WRONG');
    await click('[data-test-identity-submit]');

    assert.ok(Backups.identity.isPresent);
    assert.dom('[data-test-field-error]').includesText('no identity matched');
  });

  test('a selective restore: choose items, preview, overwrite a Variables path, restore', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const backup = server.db.norcaBackups[1];
    await visit(`/backups/${backup.ID}?tab=contents`);
    await unlock();

    await click('[data-test-select-item="Job api (default)"]');
    await click('[data-test-tab="variables"]');
    await click(
      '[data-test-select-item="Variables path nomad/jobs/api (default)"]'
    );
    assert.dom('[data-test-restore-selection]').includesText('job api');
    await click('[data-test-restore-selected]');

    assert.strictEqual(
      currentURL(),
      `/backups/${backup.ID}/restore?mode=selective`
    );
    assert.dom('[data-test-step-items]').exists();
    assert.dom('[data-test-restore-item]').exists({ count: 2 });
    await a11yAudit(assert, STRICT);

    await click('[data-test-to-preview]');
    let body = JSON.parse(
      lastRequest('POST', `/backup/${backup.ID}/restore`).requestBody
    );
    assert.true(body.DryRun);
    assert.deepEqual(
      body.Items.map((i) => i.Type),
      ['job', 'variable']
    );
    assert
      .dom('[data-test-step-preview]')
      .includesText('as a new version of job "api"');
    assert.dom('[data-test-overwrite]').exists({ count: 1 });
    await click('[data-test-overwrite] input');

    await click('[data-test-to-confirm]');
    assert.dom('[data-test-step-confirm]').exists();
    await click('[data-test-run-restore]');

    body = JSON.parse(
      lastRequest('POST', `/backup/${backup.ID}/restore`).requestBody
    );
    assert.false(body.DryRun);
    assert.strictEqual(body.Items[1].CheckIndex, 310);
    assert
      .dom('[data-test-result-summary]')
      .includesText('All 2 items were written');
  });

  test('comparing with the current state lists added, deleted and modified objects and a job diff', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const backup = server.db.norcaBackups[1];
    await visit(`/backups/${backup.ID}?tab=compare&type=jobs`);
    await unlock();

    assert.dom('[data-test-change="added"]').exists({ count: 1 });
    assert.dom('[data-test-change="deleted"]').exists({ count: 1 });
    assert.dom('[data-test-change="modified"]').exists({ count: 1 });
    await click('[data-test-job-diff="api"]');
    assert.dom('[data-test-job-diff-table]').includesText('Meta[version]');
    await a11yAudit(assert, STRICT);
  });

  test('a full restore needs a management token', async function (assert) {
    useToken({ Backup: { Policy: 'write' } });
    seedBackups(server);
    await Backups.visitRestore({ id: server.db.norcaBackups[0].ID });
    assert.dom('[data-test-mode-full]').doesNotExist();
    assert.dom('[data-test-full-not-permitted]').exists();
  });

  test('a full restore: preview, typed region name, request waiting for a second administrator', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const backup = server.db.norcaBackups[1];
    await Backups.visitRestore({ id: backup.ID });
    await click('[data-test-mode-full]');
    await unlock();

    assert
      .dom('[data-test-full-impact]')
      .includesText('4 running allocations stop');
    assert.dom('[data-test-count-removed="jobs"]').hasText('1');
    assert.dom('[data-test-full-warning]').exists();
    await a11yAudit(assert, STRICT);

    await click('[data-test-to-full-confirm]');
    assert.dom('[data-test-request-full]').isDisabled();
    await fillIn('[data-test-full-typed]', 'global');
    assert.dom('[data-test-request-full]').isNotDisabled();
    await click('[data-test-request-full]');

    const body = JSON.parse(
      lastRequest('POST', `/backup/${backup.ID}/restore`).requestBody
    );
    assert.strictEqual(body.Mode, 'full');
    assert.strictEqual(body.Confirm, 'global');
    assert
      .dom('[data-test-step-approval]')
      .includesText('second management token');

    await click('[data-test-open-request]');
    assert.dom('[data-test-restore-status]').hasText('pending');
    assert.dom('[data-test-requester-note]').exists();
    assert.dom('[data-test-approve]').doesNotExist();
    assert.dom('[data-test-reject]').hasText('Withdraw');
    await a11yAudit(assert, STRICT);
  });

  function pendingRestore(backup) {
    return collection(server.db, 'norcaBackupRestores').insert({
      ID: 'restore-9-0000-0000-000000000000',
      BackupID: backup.ID,
      Mode: 'full',
      Items: [],
      Status: 'pending',
      StatusDescription: 'waiting for approval',
      RequestedBy: 'token:someone-else',
      RequestedByName: 'Alice',
      ExpiresAt: new Date(Date.now() + 3600000).toISOString(),
      Preview: {
        Region: 'global',
        BackupIndex: 1,
        CurrentIndex: 2,
        Counts: { 'jobs/removed': 2 },
        AllocationsStopped: 7,
        ACLTokensRemoved: 1,
        Warnings: ['every change since the backup is lost'],
        Objects: [],
      },
      CreateTime: new Date().toISOString(),
      ModifyTime: new Date().toISOString(),
    });
  }

  test('a second administrator approves a full restore with the identity and the typed region', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    pendingRestore(server.db.norcaBackups[1]);

    await Backups.visit();
    assert.dom('[data-test-pending-restore]').includesText('Alice requested');
    await click('[data-test-review-restore]');
    assert.dom('[data-test-request-preview]').includesText('7');

    await click('[data-test-approve]');
    assert.dom('[data-test-confirm-submit]').isDisabled();
    await unlock();
    await fillIn('[data-test-confirm-typed]', 'global');
    await click('[data-test-confirm-submit]');

    const body = JSON.parse(lastRequest('POST', '/approve').requestBody);
    assert.strictEqual(body.AgeIdentity, backupIdentity);
    assert.dom('[data-test-restore-status]').hasText('complete');
    assert.dom('[data-test-request-details]').includesText('Undo with backup');
  });

  test('a full restore request can be rejected with a reason', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    const restore = pendingRestore(server.db.norcaBackups[1]);
    await Backups.visitRequest({ id: restore.ID });
    await click('[data-test-reject]');
    await fillIn('[data-test-reject-reason]', 'wrong backup');
    await click('[data-test-confirm-submit]');

    assert.strictEqual(
      JSON.parse(lastRequest('POST', '/reject').requestBody).Reason,
      'wrong backup'
    );
    assert.dom('[data-test-restore-status]').hasText('rejected');
    assert
      .dom('[data-test-restore-description]')
      .hasText('rejected: wrong backup');
  });

  test('job history finds old versions in the backups and restores one as a new version', async function (assert) {
    useToken(null, 'management');
    seedBackups(server);
    server.create('job', {
      id: 'api',
      createAllocations: false,
      shallow: true,
    });
    await visit('/backups/job-history?job=api&namespace=default');
    await unlock();

    assert.dom('[data-test-history-results]').exists();
    assert.dom('[data-test-history-version]').exists({ count: 3 });
    await click('[data-test-tab="deployments"]');
    assert.dom('[data-test-history-results] [data-test-ui-table-row]').exists();
    await click('[data-test-tab="versions"]');
    await a11yAudit(assert, STRICT);

    await click('[data-test-restore-version="38"]');
    assert.ok(currentURL().includes('/restore?mode=selective'));
    assert.dom('[data-test-restore-item]').hasText('Job api v38 (default)');
  });
});
