/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { ownerFromMeta, parseObjectOwner } from 'nomad-ui/utils/job-owner';
import {
  ownerMetaValue,
  ownerLink,
  ownsObject,
  deletionState,
  objectHealth,
} from 'nomad-ui/utils/objects';
import {
  jobSecretReferences,
  latestAccesses,
  accessorLabel,
  validSecretPath,
  validItemKey,
  shortHash,
  formatInterval,
} from 'nomad-ui/utils/secrets';
import { allowsSecret } from 'nomad-ui/utils/norca-acl';
import { summarizeSeal, buildWarnings } from 'nomad-ui/utils/dashboard-data';

function token(rules, type = 'client') {
  return {
    aclEnabled: true,
    selfToken: { type },
    selfTokenPolicies: rules.map((rulesJSON) => ({ rulesJSON })),
  };
}

module('Unit | Utility | objects and secrets', function () {
  test('the owner meta of a job is parsed into a link to its object', function (assert) {
    const value = ownerMetaValue({
      group: 'db.example.com',
      kind: 'PostgresCluster',
      namespace: 'prod',
      name: 'billing',
    });
    assert.strictEqual(
      value,
      'prod/crd/db-example-com/PostgresCluster/billing'
    );
    assert.deepEqual(
      parseObjectOwner(value, 'db.example.com/PostgresCluster'),
      {
        namespace: 'prod',
        kindName: 'PostgresCluster',
        name: 'billing',
        group: 'db.example.com',
      }
    );
    const owner = ownerFromMeta({
      'norca.owner': value,
      'norca.owner.kind': 'db.example.com/PostgresCluster',
    });
    assert.strictEqual(owner.kind, 'PostgresCluster');
    assert.strictEqual(owner.name, 'billing');
    assert.strictEqual(owner.route, 'objects.object');
    assert.deepEqual(owner.models, [
      'db.example.com',
      'PostgresCluster',
      'billing',
    ]);
    assert.deepEqual(owner.query, { namespace: 'prod' });

    const noKind = ownerFromMeta({ 'norca.owner': value });
    assert.strictEqual(noKind.name, 'billing');
    assert.notOk(noKind.route, 'without the kind there is no link');

    const release = ownerFromMeta({
      'norca.owner': 'web/crd/packs-norca-io/Release/shop',
      'norca.owner.kind': 'packs.norca.io/Release',
    });
    assert.strictEqual(release.kind, 'release');
    assert.deepEqual(release.models, ['web', 'shop']);

    assert.strictEqual(
      ownerFromMeta({ 'pack.release': 'shop', 'pack.release_namespace': 'web' })
        .route,
      'packages.release'
    );
    assert.strictEqual(ownerFromMeta({ team: 'x' }), null);
  });

  test('owner references of objects', function (assert) {
    const parent = {
      ID: 'p1',
      Group: 'db.example.com',
      Kind: 'PostgresCluster',
      Namespace: 'prod',
      Name: 'billing',
    };
    const ref = {
      Kind: 'db.example.com/PostgresCluster',
      Name: 'billing',
      ID: 'p1',
    };
    assert.true(ownsObject(ref, { Namespace: 'prod' }, parent));
    assert.false(
      ownsObject(ref, { Namespace: 'dev' }, parent),
      'an owner without namespace is in the namespace of the object'
    );
    assert.false(
      ownsObject({ ...ref, ID: 'other' }, { Namespace: 'prod' }, parent)
    );
    assert.true(
      ownsObject({ ...ref, Namespace: 'prod' }, { Namespace: '' }, parent),
      'a cluster-scoped object names the namespace'
    );

    assert.deepEqual(
      ownerLink({ Kind: 'Job', Name: 'web' }, { Namespace: 'prod' }),
      {
        label: 'Job web',
        route: 'jobs.job.index',
        models: ['web@prod'],
      }
    );
    assert.strictEqual(
      ownerLink(ref, { Namespace: 'prod' }).route,
      'objects.object'
    );
    assert.notOk(ownerLink({ Kind: 'Variable', Name: 'x' }, {}).route);
  });

  test('the deletion state says what an object waits for', function (assert) {
    assert.strictEqual(deletionState({}), null);
    const now = Date.now() * 1e6;
    assert.ok(
      deletionState({
        DeletionTime: now,
        Finalizers: ['a/b'],
      }).waiting.includes('a/b')
    );
    assert.ok(
      deletionState({
        DeletionTime: now,
        DeletionPropagation: 'foreground',
      }).waiting.includes('owns')
    );
    assert.strictEqual(
      objectHealth({ DeletionTime: now, Generation: 1 }).label,
      'deleting'
    );
  });

  test('the jobs that reference a secret path', function (assert) {
    const job = {
      ID: 'billing',
      TaskGroups: [
        {
          Name: 'api',
          SecretAccess: [{ Path: 'jobs/${job}/*' }],
          Tasks: [
            {
              Name: 'app',
              Secrets: [
                { Name: 'db', Path: 'prod/db/postgres' },
                { Name: 'ext', Provider: 'bao', Path: 'prod/db/postgres' },
                { Name: 'own', Path: 'jobs/${job}/${task}' },
              ],
              Templates: [
                {
                  DestPath: 'local/stripe.env',
                  EmbeddedTmpl:
                    '{{ with nomadSecret "prod/api/stripe" }}{{ .api_key }}{{ end }}',
                },
              ],
            },
          ],
        },
      ],
    };
    const refs = (path) =>
      jobSecretReferences(job, path).map((r) => `${r.where}: ${r.how}`);
    assert.deepEqual(refs('prod/db/postgres'), ['task api/app: secret "db"']);
    assert.deepEqual(refs('jobs/billing/app'), [
      'group api: secret_access jobs/${job}/*',
      'task api/app: secret "own"',
    ]);
    assert.deepEqual(refs('prod/api/stripe'), [
      'task api/app: template local/stripe.env',
    ]);
    assert.deepEqual(refs('other'), []);
  });

  test('the latest access per secret from audit records', function (assert) {
    const events = [
      {
        Topic: 'SecretAccess',
        Payload: {
          Time: '2026-10-26T10:00:00Z',
          Namespace: 'default',
          Path: 'a',
          Operation: 'read',
        },
      },
      {
        Topic: 'SecretAccess',
        Payload: {
          Time: '2026-10-26T11:00:00Z',
          Namespace: 'default',
          Path: 'a',
          Operation: 'write',
        },
      },
      { Topic: 'Job', Payload: {} },
    ];
    const latest = latestAccesses(events);
    assert.strictEqual(latest.size, 1);
    assert.strictEqual(latest.get('default|a').Operation, 'write');
    assert.strictEqual(
      accessorLabel({ JobID: 'web', TaskGroup: 'g', Task: 't' }),
      'task web/g/t'
    );
    assert.strictEqual(
      accessorLabel({ AccessorID: '12345678-aaaa' }),
      'token 12345678'
    );
  });

  test('secret paths, item keys, hashes and intervals', function (assert) {
    assert.true(validSecretPath('prod/db/postgres'));
    assert.false(validSecretPath('/prod'));
    assert.false(validSecretPath('prod//db'));
    assert.false(validSecretPath('prod/../db'));
    assert.true(validItemKey('db.password'));
    assert.false(validItemKey('a b'));
    assert.strictEqual(
      shortHash('hmac-sha256:0123456789abcdef'),
      '0123456789ab…'
    );
    assert.strictEqual(formatInterval(30 * 86400e9), '30 days');
    assert.strictEqual(formatInterval(90 * 60e9), '1.5 hours');
  });

  test('secret capabilities follow the namespace and the path rules', function (assert) {
    const t = token([
      {
        Namespaces: [
          {
            Name: 'prod',
            Capabilities: ['list-jobs'],
            Secrets: [
              { PathSpec: 'app/*', Capabilities: ['read-secret'] },
              { PathSpec: 'app/admin', Capabilities: ['deny'] },
            ],
          },
          { Name: 'dev', Capabilities: ['write-secret', 'list-secrets'] },
        ],
      },
    ]);
    assert.true(allowsSecret(t, 'prod', 'app/db', 'read-secret'));
    assert.true(
      allowsSecret(t, 'prod', 'app/db', 'list-secrets'),
      'read-secret implies list-secrets'
    );
    assert.false(allowsSecret(t, 'prod', 'app/db', 'write-secret'));
    assert.false(allowsSecret(t, 'prod', 'app/admin', 'list-secrets'));
    assert.false(allowsSecret(t, 'prod', 'other', 'read-secret'));
    assert.true(allowsSecret(t, 'dev', 'anything', 'write-secret'));
    assert.true(allowsSecret(t, '*', '*', 'list-secrets'));
    assert.false(allowsSecret(t, '*', '*', 'destroy-secret'));
    assert.true(
      allowsSecret(token([], 'management'), 'x', 'y', 'destroy-secret')
    );
  });

  test('the seal summary and its dashboard warnings', function (assert) {
    assert.strictEqual(summarizeSeal(null), null);
    const aead = summarizeSeal({ Config: null, Servers: [{ Sealed: false }] });
    assert.strictEqual(aead.types, 'aead (no seal configured)');
    assert.true(aead.aead);
    const sealed = summarizeSeal({
      Config: { Seals: [{ Type: 'shares', Threshold: 3, Shares: 5 }] },
      Servers: [{ Sealed: true }, { Sealed: false }],
    });
    assert.strictEqual(sealed.types, 'shares (3 of 5)');
    assert.strictEqual(sealed.sealed, 1);
    const summary = {
      region: 'eu',
      leader: 'x',
      servers: { total: 1, alive: 1 },
      clients: { down: 0, disconnected: 0 },
      blockedEvaluations: [],
      quotas: [],
      releases: [],
      failedDeployments: [],
      backup: null,
      seal: sealed,
    };
    const warnings = buildWarnings([summary]);
    assert.strictEqual(warnings[0].title, '1 server is sealed in eu.');
    assert.strictEqual(warnings[0].route, 'secrets');
    const recovery = buildWarnings([
      { ...summary, seal: { ...sealed, needsRecovery: true } },
    ]);
    assert.ok(recovery[0].title.includes('needs recovery'));
  });
});
