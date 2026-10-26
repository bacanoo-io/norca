/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// The owner of a job, from its meta: a release of the package manager
// (pack.release, pack.release_namespace) or an operator object
// (norca.owner, norca.owner.kind). Jobs with an owner are changed through
// their owner; a manual change is overwritten at the next reconcile.

const RELEASE = 'pack.release';
const RELEASE_NAMESPACE = 'pack.release_namespace';
const OBJECT = 'norca.owner';
const OBJECT_KIND = 'norca.owner.kind';

export function ownerFromMeta(meta) {
  if (!meta) return null;
  if (meta[RELEASE]) {
    return {
      kind: 'release',
      name: meta[RELEASE],
      namespace: meta[RELEASE_NAMESPACE] || null,
    };
  }
  if (meta[OBJECT]) {
    return {
      kind: meta[OBJECT_KIND] || 'object',
      name: meta[OBJECT],
      namespace: null,
    };
  }
  return null;
}

export function jobOwner(job) {
  const meta = job?.get ? job.get('meta.raw') : job?.Meta;
  return ownerFromMeta(meta);
}
