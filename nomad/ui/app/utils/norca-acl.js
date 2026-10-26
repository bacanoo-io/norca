/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { get } from '@ember/object';

// What the token may do in the new feature areas, read from the parsed
// rules of its policies (RulesJSON): the operator-level blocks (backup,
// quota, admission, operator) and the namespace capabilities, including the
// object rules of a namespace. Pages hide or disable the actions the token
// cannot perform; the server checks again.

const LEVELS = { deny: -1, read: 1, write: 2 };

function policies(token) {
  const list = token.selfTokenPolicies;
  if (!list) return [];
  return typeof list.toArray === 'function' ? list.toArray() : [...list];
}

export function isUnrestricted(token) {
  return !token.aclEnabled || get(token, 'selfToken.type') === 'management';
}

export function isManagement(token) {
  return get(token, 'selfToken.type') === 'management';
}

// `block` is a key of RulesJSON: Backup, Quota, Admission, Operator, Agent...
// `level` is read or write; a deny in any policy wins.
export function allowsBlock(token, block, level = 'read') {
  if (isUnrestricted(token)) return true;
  let best = 0;
  for (const policy of policies(token)) {
    const value = get(policy, `rulesJSON.${block}.Policy`);
    if (!value) continue;
    if (value === 'deny') return false;
    best = Math.max(best, LEVELS[value] || 0);
  }
  return best >= LEVELS[level];
}

function globMatches(pattern, value) {
  if (pattern === value) return true;
  if (!pattern.includes('*')) return false;
  const re = new RegExp(
    `^${pattern
      .split('*')
      .map((p) => p.replace(/[.+?^${}()|[\]\\]/g, '\\$&'))
      .join('.*')}$`
  );
  return re.test(value);
}

// The namespace rules that apply to a namespace: the exact one, else the
// closest glob (longest pattern). With namespace "*" every rule applies
// (the page spans all namespaces).
function namespaceRules(token, namespace) {
  const rules = [];
  for (const policy of policies(token)) {
    const namespaces = get(policy, 'rulesJSON.Namespaces') || [];
    if (namespace === '*') {
      rules.push(...namespaces);
      continue;
    }
    const exact = namespaces.find((n) => n.Name === namespace);
    if (exact) {
      rules.push(exact);
      continue;
    }
    const glob = namespaces
      .filter((n) => n.Name.includes('*') && globMatches(n.Name, namespace))
      .sort((a, b) => b.Name.length - a.Name.length)[0];
    if (glob) rules.push(glob);
  }
  return rules;
}

// One of `capabilities` in the namespace (or in any namespace for "*").
export function allowsNamespace(token, namespace, capabilities) {
  if (isUnrestricted(token)) return true;
  const wanted = [].concat(capabilities);
  return namespaceRules(token, namespace || 'default').some((rule) => {
    const caps = rule.Capabilities || [];
    if (caps.includes('deny')) return false;
    return wanted.some((c) => caps.includes(c));
  });
}

// A secret capability (list-secrets, read-secret, write-secret,
// destroy-secret) on a secret path in the namespace, like the server
// (acl.AllowSecretOperation): the namespace capabilities or the most
// specific secret path rule grant it, a deny in either refuses it, and
// read-secret implies list-secrets. With path "*" any rule granting the
// capability on some path counts (the page lists what is visible).
export function allowsSecret(token, namespace, path, capability) {
  if (isUnrestricted(token)) return true;
  const grants = (caps) =>
    caps.includes(capability) ||
    (capability === 'list-secrets' && caps.includes('read-secret'));
  return namespaceRules(token, namespace || 'default').some((rule) => {
    const nsCaps = rule.Capabilities || [];
    if (nsCaps.includes('deny')) return false;
    const rules = (rule.Secrets || []).filter((s) => s.PathSpec);
    if (path === '*') {
      return (
        grants(nsCaps) ||
        rules.some((s) => {
          const caps = s.Capabilities || [];
          return !caps.includes('deny') && grants(caps);
        })
      );
    }
    const exact = rules.find((s) => s.PathSpec === path);
    const match =
      exact ||
      rules
        .filter(
          (s) => s.PathSpec.includes('*') && globMatches(s.PathSpec, path)
        )
        .sort((a, b) => b.PathSpec.length - a.PathSpec.length)[0];
    const pathCaps = match?.Capabilities || [];
    if (pathCaps.includes('deny')) return false;
    return grants(nsCaps) || grants(pathCaps);
  });
}

// An object capability (read, write, ...) on a kind ("group/Kind") in the
// namespace. Object rules accept a policy shorthand (write = read + write).
export function allowsObject(token, namespace, kind, capability = 'read') {
  if (isUnrestricted(token)) return true;
  return namespaceRules(token, namespace || 'default').some((rule) =>
    (rule.Objects || []).some((o) => {
      if (!globMatches(o.Kind, kind)) return false;
      const caps = [...(o.Capabilities || [])];
      if (o.Policy === 'read') caps.push('read');
      if (o.Policy === 'write') caps.push('read', 'write');
      if (caps.includes('deny')) return false;
      return caps.includes(capability);
    })
  );
}
