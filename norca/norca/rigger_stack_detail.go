// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"net/http"
	"strings"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
)

// RiggerStackDetailResponse is what the form of a published stack of a
// registry needs of a version (design/rig-integration.md 4.8d): Source is
// the address a stack request names it by, with the version.
type RiggerStackDetailResponse struct {
	Source     string
	Metadata   *module.Metadata
	Readme     string
	FormSchema map[string]any
	// Versions are the versions of the stack in the registry, the newest
	// first.
	Versions []string
	// Meets reports whether the cluster meets the requirements of the
	// version; Unmet lists what it lacks.
	Meets bool
	Unmet []string
	// Warnings are the advisories of the version.
	Warnings []string
	structs.QueryMeta
}

// StackDetail reads a version of a published stack of a synced registry:
// the stack is fetched and verified as an apply fetches it. It needs read
// on registries; the name of the request is <namespace>/<name> in the
// global registry, <name> in a registry repository.
func (r *Rigger) StackDetail(args *RiggerDetailRequest, reply *RiggerStackDetailResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.StackDetail", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.StackDetail", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	return r.srv.riggerStackDetail(args, reply)
}

func (s *Server) riggerStackDetail(args *RiggerDetailRequest, reply *RiggerStackDetailResponse) error {
	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	reg, err := s.syncedRegistry(args.Registry)
	if err != nil {
		return err
	}

	src, err := s.newRiggerSources(args)
	if err != nil {
		return err
	}

	if ctx, err = src.use(ctx, reg.Status.Source); err != nil {
		return err
	}

	idx, err := src.registries.read(ctx, reg.Spec.Type, reg.Status.Source, reg.Status.Commit)
	if err != nil {
		return err
	}

	ref, address, err := stackRef(reg, args.Module)
	if err != nil {
		return err
	}

	versions, err := itemVersions(idx.items, ref)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s of the registry %q: %v", ref, reg.Name, err)
	}

	v, err := newestMeeting(versions, args.Version)
	if err != nil {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s of the registry %q at its last sync: %v (rigger registry sync %s fetches newer versions)", ref, reg.Name, err, reg.Name)
	}

	eng, err := s.riggerFetchEngine(src)
	if err != nil {
		return err
	}

	// Its modules may come from other registries: the rules of the sources
	// of the request hold, as in an apply.
	st, diags := eng.FetchStack(ctx, address, v)
	if diags.HasErrors() {
		if err := src.refusal(); err != nil {
			return err
		}

		return riggerDiagsError(diags)
	}

	reply.Source, reply.Versions = address, versions
	reply.Metadata, reply.FormSchema = st.Metadata(), st.FormSchema()

	if readme, err := st.Readme(); err == nil {
		reply.Readme = string(readme)
	}

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			reply.Warnings = append(reply.Warnings, strings.TrimSpace(d.Summary+": "+d.Detail))
		}
	}

	reply.Meets = true
	if md := reply.Metadata; md != nil {
		reply.Meets, reply.Unmet = s.evaluatePackCompatibility(&structs.ReleasePackRequires{
			Norca: md.Requires.Norca, Features: md.Requires.Features,
		})
	}

	reply.Index = max(1, s.raft.AppliedIndex())

	return nil
}

// stackRef is the reference of a stack of a registry in its index, and the
// address a stack request names it by.
func stackRef(reg *RiggerRegistry, name string) (catalog.Ref, string, error) {
	ref := catalog.Ref{Kind: index.KindStack, Name: name}

	if reg.Spec.Type != riggerRegistryTypeGlobal {
		return ref, strings.TrimRight(reg.Status.Source, "/") + "//stacks/" + name, nil
	}

	ns, n, ok := strings.Cut(name, "/")
	if !ok {
		return ref, "", structs.NewErrRPCCodedf(http.StatusBadRequest, "a stack of the global registry is <namespace>/<name>, not %q", name)
	}

	ref.Namespace, ref.Name = ns, n

	return ref, name, nil
}
