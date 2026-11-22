// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"net/http"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/state"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// The use of a private repository. A repository that a registry with an
// authSecret has is used only with read on that Variable, beside the rights
// on the registries: in a sync, a search, a detail, an icon, an install
// and a stack, also when the cache or a bundle serves it. The rule is the
// repository's, whichever registry or entry of the global registry names
// it. The credentials go with such a use, in its context: the fetcher gets
// none for a URL alone.

// riggerCredentialKey is the key of a riggerCredential in a context.
type riggerCredentialKey struct{}

// riggerCredential allows the credentials of a Variable for one repository.
type riggerCredential struct {
	repository string
	secret     *RiggerRegistryAuthSecret
}

// useRiggerRepository checks the use of a repository by the caller, and
// returns the context in which the fetcher gets its credentials.
func (s *Server) useRiggerRepository(ctx context.Context, aclObj *acl.ACL, repository string) (context.Context, error) {
	regs, err := s.riggerRegistryList()
	if err != nil {
		return ctx, err
	}

	want := fetch.CanonicalRepository(repository)
	out := ctx

	for _, reg := range regs {
		a := reg.Spec.AuthSecret
		if a == nil {
			continue
		}

		now := fetch.CanonicalRepository(reg.Spec.Source) == want
		atLastSync := reg.Status.Source != "" && fetch.CanonicalRepository(reg.Status.Source) == want

		if !now && !atLastSync {
			continue
		}

		if !canReadRiggerAuth(aclObj, a) {
			return ctx, structs.NewErrRPCCodedf(http.StatusForbidden,
				"%v: the repository of the registry %q needs read on the Variable %q in the namespace %q",
				structs.ErrPermissionDenied, reg.Name, a.Path, riggerAuthNamespace(a))
		}

		// The Variable was given for the source the registry has now.
		if now {
			out = context.WithValue(ctx, riggerCredentialKey{}, riggerCredential{repository: want, secret: a})
		}
	}

	return out, nil
}

// credentialOf returns the Variable whose credentials the context allows
// for a repository, nil for none.
func credentialOf(ctx context.Context, repository string) *RiggerRegistryAuthSecret {
	c, ok := ctx.Value(riggerCredentialKey{}).(riggerCredential)
	if !ok || c.repository != fetch.CanonicalRepository(repository) {
		return nil
	}

	return c.secret
}

// canReadRiggerAuth reports whether the caller may read the Variable of an
// authSecret.
func canReadRiggerAuth(aclObj *acl.ACL, a *RiggerRegistryAuthSecret) bool {
	return aclObj == nil || aclObj.AllowVariableOperation(riggerAuthNamespace(a), a.Path, acl.VariablesCapabilityRead, nil)
}

// riggerRegistryList returns the registries of the region, by name.
func (s *Server) riggerRegistryList() ([]*RiggerRegistry, error) {
	iter, err := s.State().CustomObjects(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", state.SortDefault)
	if err != nil {
		return nil, err
	}

	var out []*RiggerRegistry

	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj, ok := raw.(*structs.CustomObject)
		if !ok {
			continue
		}

		reg, err := riggerRegistryFromObject(obj)
		if err != nil {
			return nil, err
		}

		out = append(out, reg)
	}

	return out, nil
}
