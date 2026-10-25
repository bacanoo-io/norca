/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { Response } from 'miragejs';

// Helpers of the Mirage routes of the new feature areas. Their data lives in
// plain db collections (no Ember Data models): tests and scenarios insert
// the API's JSON shapes directly, e.g. server.db.norcaBackups.insert({...}).

export function collection(db, name) {
  if (!db[name]) db.createCollection(name);
  return db[name];
}

export function body(request) {
  if (!request.requestBody) return {};
  try {
    return JSON.parse(request.requestBody);
  } catch (e) {
    return {};
  }
}

// Strips Mirage's own id from a stored record.
export function plain(record) {
  if (!record) return record;
  // eslint-disable-next-line no-unused-vars
  const { id, ...rest } = record;
  return rest;
}

export function error(status, text) {
  return new Response(status, { 'Content-Type': 'text/plain' }, text);
}

export function json(data, index = 1) {
  return new Response(200, { 'x-nomad-index': String(index) }, data);
}

export function inNamespace(record, namespace, field = 'Namespace') {
  if (!namespace || namespace === '*') return true;
  return (record[field] || 'default') === namespace;
}
