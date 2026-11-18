// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca-rig/host"
	"github.com/bacanoo-io/norca-rig/module"
	"github.com/bacanoo-io/norca-rig/resource"
)

// ownerPrefix marks the stack that owns a release in its description (the
// spike of phase 0, design 12.2): "rig stack <name>". A release installed
// on its own is described as "rig".
const (
	ownerPrefix = "rig stack "
	ownDesc     = "rig"
)

// Releases is the release API of Norca (host.Releases).
type Releases struct {
	client *api.Client
}

var _ host.Releases = (*Releases)(nil)

// NewReleases returns the release API of the cluster of client.
func NewReleases(client *api.Client) *Releases {
	return &Releases{client: client}
}

func query(ctx context.Context, namespace string) *api.QueryOptions {
	return (&api.QueryOptions{Namespace: namespace}).WithContext(ctx)
}

func write(ctx context.Context, namespace string) *api.WriteOptions {
	return (&api.WriteOptions{Namespace: namespace}).WithContext(ctx)
}

// notFound maps a 404 of the API onto host.ErrNotFound.
func notFound(err error) error {
	var resp api.UnexpectedResponseError
	if errors.As(err, &resp) && resp.StatusCode() == http.StatusNotFound {
		return fmt.Errorf("%w: %w", host.ErrNotFound, err)
	}

	return err
}

// Current returns the current revision of a release.
func (r *Releases) Current(ctx context.Context, ref host.ReleaseRef) (host.Revision, error) {
	rel, _, err := r.client.Releases().Info(ref.Name, query(ctx, ref.Namespace))
	if err != nil {
		return host.Revision{}, notFound(err)
	}

	if rel.Spec == nil || rel.Spec.Revision == 0 {
		return host.Revision{}, fmt.Errorf("%w: release %s/%s has no revision", host.ErrNotFound, ref.Namespace, ref.Name)
	}

	rev, _, err := r.client.Releases().Revision(ref.Name, rel.Spec.Revision, query(ctx, ref.Namespace))
	if err != nil {
		return host.Revision{}, notFound(err)
	}

	return revision(ref, rev), nil
}

// History returns the revisions of a release, newest first.
func (r *Releases) History(ctx context.Context, ref host.ReleaseRef) ([]host.Revision, error) {
	revs, _, err := r.client.Releases().History(ref.Name, query(ctx, ref.Namespace))
	if err != nil {
		return nil, notFound(err)
	}

	out := make([]host.Revision, 0, len(revs))
	for _, rev := range revs {
		out = append(out, revision(ref, rev))
	}

	slices.SortFunc(out, func(a, b host.Revision) int { return cmp.Compare(b.Number, a.Number) })

	return out, nil
}

// Owned returns the current revisions of the releases of every namespace
// whose description names the stack.
func (r *Releases) Owned(ctx context.Context, owner string) ([]host.Revision, error) {
	rels, _, err := r.client.Releases().List(query(ctx, "*"))
	if err != nil {
		return nil, err
	}

	var out []host.Revision

	for _, rel := range rels {
		if rel.Spec == nil || rel.Spec.Description != ownerPrefix+owner {
			continue
		}

		rev := host.Revision{
			Release: host.ReleaseRef{Namespace: rel.Namespace, Name: rel.Name},
			Number:  rel.Spec.Revision,
			Owner:   owner,
			Module:  moduleRef(rel.Spec.Pack),
		}

		if rel.Status != nil {
			rev.Status = rel.Status.Phase
		}

		out = append(out, rev)
	}

	return out, nil
}

// Plan returns what the server would change for the request.
func (r *Releases) Plan(ctx context.Context, req host.ApplyRequest) (host.Plan, error) {
	areq, err := applyRequest(req)
	if err != nil {
		return host.Plan{}, err
	}

	resp, _, err := r.client.Releases().Plan(req.Release.Name, areq, write(ctx, req.Release.Namespace))
	if err != nil {
		return host.Plan{}, err
	}

	plan := host.Plan{Release: req.Release}

	for _, ch := range resp.Changes {
		if ch == nil || ch.Action == "unchanged" || ch.Action == "" {
			continue
		}

		plan.Changes = append(plan.Changes, host.Change{Kind: resource.Kind(ch.Type), Name: ch.Name, Action: host.Action(ch.Action)})
	}

	return plan, nil
}

// Drift returns the resources of a release that differ in the cluster.
func (r *Releases) Drift(ctx context.Context, ref host.ReleaseRef) (host.Drift, error) {
	resp, _, err := r.client.Releases().Drift(ref.Name, write(ctx, ref.Namespace))
	if err != nil {
		return host.Drift{}, notFound(err)
	}

	d := host.Drift{Release: ref}
	for _, e := range resp.Resources {
		d.Entries = append(d.Entries, host.DriftEntry{Kind: resource.Kind(e.Type), Name: e.Name, Detail: e.Reason})
	}

	return d, nil
}

// Apply applies the request as a new revision.
func (r *Releases) Apply(ctx context.Context, req host.ApplyRequest) (host.Revision, error) {
	areq, err := applyRequest(req)
	if err != nil {
		return host.Revision{}, err
	}

	resp, _, err := r.client.Releases().Apply(req.Release.Name, areq, write(ctx, req.Release.Namespace))
	if err != nil {
		return host.Revision{}, err
	}

	rev := host.Revision{
		Release: req.Release, Number: resp.Revision, Module: req.Module, Digest: req.Digest, Owner: req.Owner,
	}

	if resp.Release != nil && resp.Release.Status != nil {
		rev.Status = resp.Release.Status.Phase
	}

	return rev, nil
}

// Rollback makes an earlier revision current again.
func (r *Releases) Rollback(ctx context.Context, ref host.ReleaseRef, number uint64) (host.Revision, error) {
	resp, _, err := r.client.Releases().Rollback(ref.Name, &api.ReleaseRollbackRequest{Revision: number, Wait: true}, write(ctx, ref.Namespace))
	if err != nil {
		return host.Revision{}, notFound(err)
	}

	return r.revisionOf(ctx, ref, resp.Revision)
}

// Uninstall removes a release and its resources.
func (r *Releases) Uninstall(ctx context.Context, ref host.ReleaseRef) error {
	_, _, err := r.client.Releases().Delete(ref.Name, &api.ReleaseDeleteOptions{Propagation: api.ReleasePropagationBackground}, write(ctx, ref.Namespace))

	return notFound(err)
}

func (r *Releases) revisionOf(ctx context.Context, ref host.ReleaseRef, number uint64) (host.Revision, error) {
	rev, _, err := r.client.Releases().Revision(ref.Name, number, query(ctx, ref.Namespace))
	if err != nil {
		return host.Revision{}, err
	}

	return revision(ref, rev), nil
}

// revision maps a revision of the API.
func revision(ref host.ReleaseRef, rev *api.ReleaseRevision) host.Revision {
	out := host.Revision{Release: ref, Number: rev.Revision}

	if s := rev.Spec; s != nil {
		out.Module = moduleRef(s.Pack)
		out.Digest = s.ValuesHash
		out.Owner = strings.TrimPrefix(s.Description, ownerPrefix)

		if !strings.HasPrefix(s.Description, ownerPrefix) {
			out.Owner = ""
		}

		if t, err := time.Parse(time.RFC3339, s.CreateTime); err == nil {
			out.CreatedAt = t
		}
	}

	if rev.Status != nil {
		out.Status = rev.Status.Phase
	}

	return out
}

func moduleRef(p *api.ReleasePack) host.ModuleRef {
	if p == nil {
		return host.ModuleRef{}
	}

	ref := host.ModuleRef{Name: p.Name, Source: p.Source, Version: p.Version, Digest: p.Digest}
	if p.TrustedSigner != "" {
		ref.Signers = []string{p.TrustedSigner}
	}

	return ref
}

// applyRequest maps a request of the engine onto the release API: the
// module in the pack fields, the digest of the release in ValuesHash, the
// owner in the description (design 12.2).
func applyRequest(req host.ApplyRequest) (*api.ReleaseApplyRequest, error) {
	out := &api.ReleaseApplyRequest{
		Pack: &api.ReleasePack{
			Name: req.Module.Name, Version: req.Module.Version, Source: req.Module.Source, Digest: req.Module.Digest,
			Requires: requires(req.Module.Requires),
		},
		Description: ownDesc,
		Values:      req.Values,
		ValuesHash:  req.Digest,
		Atomic:      req.Settings.Atomic,
		Wait:        req.Settings.Wait,
		Timeout:     req.Settings.Timeout,
		MaxHistory:  req.Settings.MaxHistory,
	}

	if req.Owner != "" {
		out.Description = ownerPrefix + req.Owner
	}

	for _, r := range req.Resources {
		rr, secret, err := releaseResource(r, req.Release.Namespace, req.Secrets[r.Name])
		if err != nil {
			return nil, err
		}

		out.Resources = append(out.Resources, rr)

		if secret != nil {
			out.Secrets = append(out.Secrets, secret)
		}
	}

	return out, nil
}

func requires(r *module.Requires) *api.ReleasePackRequires {
	if r == nil {
		return &api.ReleasePackRequires{Features: []string{}}
	}

	features := r.Features
	if features == nil {
		features = []string{}
	}

	return &api.ReleasePackRequires{Norca: r.Norca, Features: features}
}

// List returns the current revisions of the releases of a namespace, "*"
// for every namespace.
func (r *Releases) List(ctx context.Context, namespace string) ([]host.Revision, error) {
	rels, _, err := r.client.Releases().List(query(ctx, namespace))
	if err != nil {
		return nil, err
	}

	out := make([]host.Revision, 0, len(rels))

	for _, rel := range rels {
		rev := host.Revision{Release: host.ReleaseRef{Namespace: rel.Namespace, Name: rel.Name}}

		if rel.Spec != nil {
			rev.Number, rev.Module = rel.Spec.Revision, moduleRef(rel.Spec.Pack)
			rev.Owner = strings.TrimPrefix(rel.Spec.Description, ownerPrefix)

			if !strings.HasPrefix(rel.Spec.Description, ownerPrefix) {
				rev.Owner = ""
			}
		}

		if rel.Status != nil {
			rev.Status = rel.Status.Phase
		}

		out = append(out, rev)
	}

	slices.SortFunc(out, func(a, b host.Revision) int {
		return strings.Compare(a.Release.Namespace+"/"+a.Release.Name, b.Release.Namespace+"/"+b.Release.Name)
	})

	return out, nil
}
