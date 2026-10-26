/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { inject as service } from '@ember/service';
import { action } from '@ember/object';
import { modifier } from 'ember-modifier';
import { waitForPromise } from '@ember/test-waiters';
import moment from 'moment';
import { errorMessage } from 'nomad-ui/utils/norca-api';
import {
  allowsBlock,
  allowsSecret,
  isUnrestricted,
} from 'nomad-ui/utils/norca-acl';
import {
  listSecrets,
  latestAccesses,
  accessorLabel,
} from 'nomad-ui/utils/secrets';
import LocalList from 'nomad-ui/utils/local-list';

/**
 * The secrets of the built-in store in the namespace scope: paths, the
 * current version, state, owner and description, changes, and the last
 * access seen in the SecretAccess audit records (event stream, operator
 * read). Never values. The seal status of the region's servers is shown
 * below the list for tokens with operator read.
 */
export default class SecretsList extends Component {
  @service token;
  @service system;
  @service frame;
  @service eventStream;
  @service router;

  @tracked secrets = [];
  @tracked accesses = new Map();
  @tracked isLoading = true;
  @tracked error = null;
  @tracked creating = false;

  get region() {
    return this.system.activeRegion || '';
  }

  get namespace() {
    return this.frame.namespace || '*';
  }

  // New secrets go to the namespace in scope, or default for all.
  get targetNamespace() {
    return this.namespace === '*' ? 'default' : this.namespace;
  }

  get canCreate() {
    return allowsSecret(this.token, this.targetNamespace, '*', 'write-secret');
  }

  get canAudit() {
    return allowsBlock(this.token, 'Operator', 'read');
  }

  get scopeLabel() {
    const ns =
      this.namespace === '*' ? 'all namespaces' : `namespace ${this.namespace}`;
    return `${this.region || 'Local region'}, ${ns}`;
  }

  list = new LocalList({
    sortBy: 'Path',
    searchProps: ['Path', 'Namespace', 'Description', 'Owner'],
  });

  columns = [
    { key: 'Path', label: 'Path', sortable: true, hideable: false },
    { key: 'Namespace', label: 'Namespace', sortable: true },
    { key: 'CurrentVersion', label: 'Version', sortable: true },
    { key: 'state', label: 'State', sortable: true },
    { key: 'Owner', label: 'Owner', sortable: true },
    { key: 'ModifyTime', label: 'Updated', sortable: true },
    { key: 'lastAccess', label: 'Last access', sortable: true },
  ];

  get rows() {
    return this.secrets.map((s) => {
      const access = this.accesses.get(`${s.Namespace || 'default'}|${s.Path}`);
      return {
        ...s,
        state: s.Destroyed ? 'destroyed' : s.Deleted ? 'deleted' : 'active',
        stateTone: s.Destroyed ? 'danger' : s.Deleted ? 'warning' : 'success',
        updated: s.ModifyTime ? moment(s.ModifyTime / 1e6).fromNow() : '–',
        created: s.CreateTime
          ? moment(s.CreateTime / 1e6).format('YYYY-MM-DD HH:mm')
          : '',
        lastAccess: access ? access.time : 0,
        access: access
          ? {
              when: moment(access.time).fromNow(),
              what: `${access.Operation} by ${accessorLabel(access.Accessor)}${
                access.Result && access.Result !== 'ok'
                  ? ` (${access.Result})`
                  : ''
              }`,
            }
          : null,
        expiring:
          s.ExpiresAt && new Date(s.ExpiresAt).getTime() < Date.now()
            ? 'expired'
            : null,
      };
    });
  }

  get tableRows() {
    return this.list.rows(this.rows);
  }

  async load() {
    try {
      const { body, index } = await listSecrets(
        this.token,
        this.region,
        this.namespace
      );
      this.secrets = body || [];
      this.error = null;
      return index;
    } catch (e) {
      this.secrets = [];
      this.error =
        e.status === 403
          ? 'Your token cannot list secrets in this namespace: it needs list-secrets (or read-secret) on a secret path.'
          : errorMessage(e);
      return 0;
    } finally {
      this.isLoading = false;
    }
  }

  @action openCreate() {
    this.creating = true;
  }

  @action closeCreate() {
    this.creating = false;
  }

  @action created(secret) {
    this.creating = false;
    this.router.transitionTo('secrets.secret', secret.Path, {
      queryParams: { namespace: secret.Namespace || 'default' },
    });
  }

  setup = modifier((element, [region, namespace]) => {
    waitForPromise(Promise.resolve().then(() => this.load()));
    const unsubscribers = [];
    // The Secret topic carries metadata only; it is subscribed for tokens
    // that may list every path (a partial grant would refuse the shared
    // connection). The audit records need operator read.
    if (isUnrestricted(this.token)) {
      unsubscribers.push(
        this.eventStream.subscribe({
          topics: ['Secret'],
          region,
          namespace,
          onEvents: () => waitForPromise(this.load()),
          onResync: () => this.load(),
        })
      );
    }
    if (this.canAudit) {
      unsubscribers.push(
        this.eventStream.subscribe({
          topics: ['SecretAccess'],
          region,
          namespace,
          replay: true,
          onEvents: (events) => {
            this.accesses = latestAccesses(events, this.accesses);
          },
        })
      );
    }
    return () => unsubscribers.forEach((u) => u());
  });
}
