/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Service, { inject as service } from '@ember/service';
import messageFromAdapterError from 'nomad-ui/utils/message-from-adapter-error';
import { kind } from 'nomad-ui/utils/access-control/kinds';

/**
 * Shared behaviour of the Access Control pages: which records count as
 * saved, how often other records point at them, and deleting a record with
 * its notifications. Pages describe their record type by name (see
 * utils/access-control/kinds) and call into this service.
 */
export default class AclRecordsService extends Service {
  @service notifications;
  @service router;
  @service store;

  /**
   * Records that exist on the server: drafts of the "new" pages and records
   * deleted in this session are not shown in lists.
   */
  saved(records) {
    const out = [];
    for (const record of records || []) {
      if (record.isNew || record.isDeleted) continue;
      out.push(record);
    }
    return out;
  }

  /**
   * Counts, per related ID, the saved tokens whose hasMany(relation)
   * contains that ID. With `expired` the result also tells how many of
   * them have expired: { total, expired } instead of a number.
   */
  tokenLinks(tokens, relation, { expired = false } = {}) {
    const counts = new Map();
    for (const token of this.saved(tokens)) {
      for (const id of token.hasMany(relation).ids()) {
        const entry = counts.get(id) ?? { total: 0, expired: 0 };
        entry.total += 1;
        if (token.isExpired) entry.expired += 1;
        counts.set(id, entry);
      }
    }
    const result = {};
    counts.forEach((entry, id) => {
      result[id] = expired ? entry : entry.total;
    });
    return result;
  }

  /**
   * Row click handler of the list pages: opens the record unless the click
   * landed on a control inside the row.
   */
  open(type, record, event) {
    if (event?.target?.closest?.('a, button, input')) {
      return;
    }
    const { detail } = kind(type);
    this.router.transitionTo(detail.route, record[detail.param]);
  }

  /**
   * Deletes a record, removes it from the records that link to it, and
   * reports the outcome. Resolves to true on success; on failure the record
   * is rolled back and the error is shown as a sticky notification.
   */
  async remove(type, record) {
    const { noun, plural, linkedBy } = kind(type);
    const name = record.name;
    try {
      await record.destroyRecord();
    } catch (err) {
      record.rollbackAttributes();
      this.notifications.add({
        title: `Error deleting ${noun.toLowerCase()} ${name}`,
        message: messageFromAdapterError(err, `delete ${plural}`),
        color: 'critical',
        sticky: true,
      });
      return false;
    }
    for (const owner of linkedBy) {
      for (const other of this.store.peekAll(owner)) {
        other[plural].removeObject(record);
      }
    }
    this.notifications.add({
      title: `${noun} ${name} deleted`,
      color: 'success',
    });
    return true;
  }
}
