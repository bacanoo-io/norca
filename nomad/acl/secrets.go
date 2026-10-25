// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package acl

import (
	"fmt"
	"strings"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
)

// Secrets of the built-in secret store (Norca). The capabilities can be
// granted for a whole namespace, or for secret paths with globs:
//
//	namespace "prod" {
//	  capabilities = ["list-secrets"]
//
//	  secret "db/*" {
//	    capabilities = ["read-secret"]
//	  }
//	}
//
// None of them is part of the namespace policy shorthands, and unlike
// Variables there is no implicit access for workload identities: a task can
// read a secret only through a policy bound to its job.
const (
	// NamespaceCapabilityListSecrets allows listing secret paths and reading
	// their metadata (never values).
	NamespaceCapabilityListSecrets = "list-secrets"

	// NamespaceCapabilityReadSecret allows reading secret values. It implies
	// list-secrets.
	NamespaceCapabilityReadSecret = "read-secret"

	// NamespaceCapabilityWriteSecret allows creating and updating secrets,
	// and soft deleting and undeleting their versions.
	NamespaceCapabilityWriteSecret = "write-secret"

	// NamespaceCapabilityDestroySecret allows destroying secret versions and
	// whole secrets permanently.
	NamespaceCapabilityDestroySecret = "destroy-secret"
)

// SecretPathPolicy is a secret block of a namespace policy: capabilities on
// the secret paths matching PathSpec, a path that may contain globs.
type SecretPathPolicy struct {
	PathSpec     string `hcl:",key"`
	Capabilities []string
}

func isSecretCapability(cap string) bool {
	switch cap {
	case NamespaceCapabilityListSecrets, NamespaceCapabilityReadSecret,
		NamespaceCapabilityWriteSecret, NamespaceCapabilityDestroySecret:
		return true
	default:
		return false
	}
}

// parseSecretPolicies validates the secret blocks of a namespace policy.
func parseSecretPolicies(ns *NamespacePolicy) error {
	for _, sp := range ns.Secrets {
		if sp.PathSpec == "" {
			return fmt.Errorf("Invalid missing secret path in namespace %s", ns.Name)
		}
		if len(sp.Capabilities) == 0 {
			return fmt.Errorf("Invalid secret rule for %q in namespace %s: no capabilities", sp.PathSpec, ns.Name)
		}
		for _, cap := range sp.Capabilities {
			if cap != NamespaceCapabilityDeny && !isSecretCapability(cap) {
				return fmt.Errorf("Invalid secret capability '%s' for %q in namespace %s", cap, sp.PathSpec, ns.Name)
			}
		}
	}
	return nil
}

// secretsKey is the key of a secret rule in the ACL radix trees.
func secretsKey(ns, path string) []byte {
	return []byte(ns + "\x00" + path)
}

// addSecretRules adds the secret blocks of a namespace policy to the radix
// tree transactions of concrete and of glob rules.
func addSecretRules(ns *NamespacePolicy, txn, wildcardTxn *iradix.Txn[capabilitySet]) {
	nsGlob := strings.Contains(ns.Name, "*")
	for _, sp := range ns.Secrets {
		t := txn
		if nsGlob || strings.Contains(sp.PathSpec, "*") {
			t = wildcardTxn
		}
		key := secretsKey(ns.Name, sp.PathSpec)
		caps, ok := t.Get(key)
		if !ok {
			caps = make(capabilitySet)
			t.Insert(key, caps)
		}
		for _, cap := range sp.Capabilities {
			caps.Set(cap)
		}
	}
}

// matchingSecretsCapabilitySet returns the capabilities of the most specific
// secret rule matching the namespace and path.
func (a *ACL) matchingSecretsCapabilitySet(ns, path string) (capabilitySet, bool) {
	if a.secrets != nil {
		if caps, ok := a.secrets.Get(secretsKey(ns, path)); ok {
			return caps, true
		}
	}
	if a.wildcardSecrets == nil {
		return nil, false
	}
	return a.findClosestMatchingGlob(a.wildcardSecrets, ns+"\x00"+path)
}

// AllowSecretOperation checks if the operation (one of the secret
// capabilities) is allowed on the secret path in the namespace. The
// capabilities of the namespace and of the most specific matching secret
// rule are combined; a deny in either of them denies. read-secret implies
// list-secrets. The path may itself be a glob pattern (for example for event
// stream subscriptions); it is then checked literally against the rules.
func (a *ACL) AllowSecretOperation(ns, path, op string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}

	grants := func(caps capabilitySet) bool {
		return caps.Check(op) ||
			(op == NamespaceCapabilityListSecrets && caps.Check(NamespaceCapabilityReadSecret))
	}

	nsCaps, nsOK := a.matchingNamespaceCapabilitySet(ns)
	if nsOK && nsCaps.Check(NamespaceCapabilityDeny) {
		return false
	}
	pathCaps, pathOK := a.matchingSecretsCapabilitySet(ns, path)
	if pathOK && pathCaps.Check(NamespaceCapabilityDeny) {
		return false
	}
	return (nsOK && grants(nsCaps)) || (pathOK && grants(pathCaps))
}

// AllowSecretGrant checks that the token may hand the capability on the
// secret path pattern to someone else, as a job's secret_access binding
// hands read-secret to the job's workload identities. It allows only what
// the token holds itself on every path the pattern can match: the pattern
// must be allowed as a whole (by a namespace capability or a rule covering
// it), and no deny, and no rule at least as specific as the one covering
// the pattern that lacks the capability, may intersect it. The check is
// conservative: overlapping rules that can't be ordered deny the grant.
func (a *ACL) AllowSecretGrant(ns, pattern, op string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}
	if !a.AllowSecretOperation(ns, pattern, op) {
		return false
	}

	grants := func(caps capabilitySet) bool {
		return caps.Check(op) ||
			(op == NamespaceCapabilityListSecrets && caps.Check(NamespaceCapabilityReadSecret))
	}
	nsCaps, nsOK := a.matchingNamespaceCapabilitySet(ns)
	nsGrants := nsOK && grants(nsCaps)

	// The literal length of the most specific rule covering the pattern;
	// -1 if the pattern is allowed by the namespace capabilities alone.
	key := ns + "\x00" + pattern
	covering := -1
	if a.secrets != nil {
		if _, ok := a.secrets.Get([]byte(key)); ok {
			covering = len(key)
		}
	}
	if covering < 0 && a.wildcardSecrets != nil {
		for _, m := range findAllMatchingWildcards(a.wildcardSecrets, key) {
			if l := len(m.name) - strings.Count(m.name, "*"); l > covering {
				covering = l
			}
		}
	}

	ok := true
	check := func(exact bool) func(k []byte, caps capabilitySet) bool {
		return func(k []byte, caps capabilitySet) bool {
			rule := string(k)
			if rule == key || !globsIntersect(rule, key) {
				return false
			}
			if caps.Check(NamespaceCapabilityDeny) {
				ok = false
				return true
			}
			if grants(caps) || nsGrants {
				return false
			}
			// A rule without the capability wins over the covering rule
			// for the paths it matches if it is exact or at least as
			// specific.
			if exact || len(rule)-strings.Count(rule, "*") >= covering {
				ok = false
				return true
			}
			return false
		}
	}
	if a.secrets != nil {
		a.secrets.Root().Walk(check(true))
	}
	if ok && a.wildcardSecrets != nil {
		a.wildcardSecrets.Root().Walk(check(false))
	}
	return ok
}

// globsIntersect reports whether two patterns, in which "*" matches any
// sequence of characters, match a common string.
func globsIntersect(a, b string) bool {
	type pos struct{ i, j int }
	seen := map[pos]bool{}
	var walk func(i, j int) bool
	walk = func(i, j int) bool {
		p := pos{i, j}
		if v, ok := seen[p]; ok {
			return v
		}
		seen[p] = false
		var r bool
		switch {
		case i == len(a) && j == len(b):
			r = true
		case i < len(a) && a[i] == '*':
			// The star matches nothing, or one more character of b (a
			// literal, or what b's own star stands for).
			r = walk(i+1, j) || (j < len(b) && walk(i, j+1))
		case j < len(b) && b[j] == '*':
			r = walk(i, j+1) || (i < len(a) && walk(i+1, j))
		case i < len(a) && j < len(b):
			r = a[i] == b[j] && walk(i+1, j+1)
		}
		seen[p] = r
		return r
	}
	return walk(0, 0)
}

// AllowSecretSearch is a loose check that the token may list some secrets in
// the namespace ("*" for any namespace); the results must be filtered per
// secret with AllowSecretOperation.
func (a *ACL) AllowSecretSearch(ns string) bool {
	switch {
	case a == nil:
		return true
	case a.management:
		return true
	}
	listing := func(caps capabilitySet) bool {
		return !caps.Check(NamespaceCapabilityDeny) &&
			(caps.Check(NamespaceCapabilityListSecrets) || caps.Check(NamespaceCapabilityReadSecret))
	}
	if ns == AllNamespacesSentinel {
		if a.anyNamespaceAllows(listing) {
			return true
		}
	} else if caps, ok := a.matchingNamespaceCapabilitySet(ns); ok && listing(caps) {
		return true
	}

	allow := false
	walk := func(k []byte, caps capabilitySet) bool {
		name, _, _ := strings.Cut(string(k), "\x00")
		if listing(caps) && (ns == AllNamespacesSentinel || name == ns || strings.Contains(name, "*")) {
			allow = true
		}
		return allow
	}
	if a.secrets != nil {
		a.secrets.Root().Walk(walk)
	}
	if !allow && a.wildcardSecrets != nil {
		a.wildcardSecrets.Root().Walk(walk)
	}
	return allow
}
