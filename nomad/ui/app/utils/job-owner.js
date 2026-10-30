/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// The owner of a job, from its meta: a release of the package manager
// (pack.release, pack.release_namespace) or an operator object
// (norca.owner, norca.owner.kind). Jobs with an owner are changed through
// their owner; a manual change is overwritten at the next reconcile.
//
// norca.owner holds "<namespace>/crd/<group with dashes>/<Kind>/<name>"
// (structs.ObjectOwnerMetaValue) and norca.owner.kind the "group/Kind" of
// the owner, which the link to the object needs (the dashed group can't be
// turned back into the group).

const RELEASE = 'pack.release';
const RELEASE_NAMESPACE = 'pack.release_namespace';
const OBJECT = 'norca.owner';
const OBJECT_KIND = 'norca.owner.kind';

// Parses the owner reference of an object owner.
export function parseObjectOwner(value, kindRef) {
  const match = /^([^/]*)\/crd\/([^/]+)\/([^/]+)\/(.+)$/.exec(value || '');
  if (!match) return null;
  const [, namespace, , kindName, name] = match;
  let group = null;
  if (kindRef) {
    const slash = kindRef.lastIndexOf('/');
    if (slash > 0 && kindRef.slice(slash + 1) === kindName)
      group = kindRef.slice(0, slash);
  }
  return { namespace: namespace || null, kindName, name, group };
}

export function ownerFromMeta(meta) {
  if (!meta) return null;
  if (meta[RELEASE]) {
    return {
      kind: 'release',
      name: meta[RELEASE],
      namespace: meta[RELEASE_NAMESPACE] || null,
      route: 'packages.release',
      models: [meta[RELEASE_NAMESPACE] || 'default', meta[RELEASE]],
    };
  }
  if (meta[OBJECT]) {
    const parsed = parseObjectOwner(meta[OBJECT], meta[OBJECT_KIND]);
    if (!parsed) {
      return {
        kind: meta[OBJECT_KIND] || 'object',
        name: meta[OBJECT],
        namespace: null,
      };
    }
    const owner = {
      kind: parsed.kindName,
      name: parsed.name,
      namespace: parsed.namespace,
    };
    if (parsed.group === 'packs.norca.io' && parsed.kindName === 'Release') {
      return {
        ...owner,
        kind: 'release',
        route: 'packages.release',
        models: [parsed.namespace || 'default', parsed.name],
      };
    }
    if (parsed.group) {
      owner.route = 'objects.object';
      owner.models = [parsed.group, parsed.kindName, parsed.name];
      owner.query = { namespace: parsed.namespace };
    }
    return owner;
  }
  return null;
}

export function jobOwner(job) {
  const meta = job?.get ? job.get('meta.raw') : job?.Meta;
  return ownerFromMeta(meta);
}
