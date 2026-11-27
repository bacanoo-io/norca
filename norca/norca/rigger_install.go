// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/builtin"
	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/source"
	"github.com/bacanoo-io/norca/rigger/version"
)

// One module installed by the server (design/rig-integration.md 4.6a): a
// module of a synced registry, fetched and verified by the server, a
// module built into the server (builtin/<name>), or a local module sent as
// files. The server evaluates it with the engine of its hosts
// (rigger_host.go) and applies the release with the caller's token; a
// module of a registry or a built-in one is applied as rendered by the
// server, a local one as a client's.

// riggerInstallTimeout bounds an install: the fetch of the module, its
// evaluation and the apply of its release.
const riggerInstallTimeout = 10 * time.Minute

// riggerLocalSource is the source a release records for a local module.
const riggerLocalSource = "local"

// RiggerModuleRequest names the module of an install or of a detail: a
// module of a synced registry, or (an install only) a module built into
// the servers or a local module.
type RiggerModuleRequest struct {
	// Registry and Module name a module of a synced registry:
	// <namespace>/<name> in the global registry, <name> in a registry
	// repository. Module builtin/<name> without a Registry is a module
	// built into the servers (an install only). Version is a version
	// constraint; empty takes the newest.
	Registry, Module, Version string
	// Files is a local module sent as files (rigger Module.Archive); no
	// registry then.
	Files []byte
}

// riggerValues reads the values of a request: a values file of HCL, or the
// same as one JSON object; the same values either way.
func riggerValues(asHCL string, asJSON json.RawMessage) (rigger.Values, error) {
	var (
		values rigger.Values
		diags  hcl.Diagnostics
	)

	switch {
	case asHCL != "" && len(asJSON) > 0:
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "give the values as HCL or as JSON: one of them")
	case asHCL != "":
		values, diags = rigger.ParseValues("values.vars.hcl", []byte(asHCL))
	case len(asJSON) > 0:
		values, diags = rigger.ParseValuesJSON("values.json", asJSON)
	}

	if diags.HasErrors() {
		return nil, riggerDiagsError(diags)
	}

	return values, nil
}

// RiggerInstallRequest installs or upgrades one release of a module.
type RiggerInstallRequest struct {
	Release string
	RiggerModuleRequest
	// Values are the values of the variables, as a values file (HCL);
	// ValuesJSON the same as one JSON object, as a form sends them. One of
	// them at most.
	Values     string
	ValuesJSON json.RawMessage `json:",omitempty"`
	// Plan returns what an apply would do and applies nothing.
	Plan bool
	// Adopt takes over a release another manager applied or another stack
	// owns.
	Adopt bool
	// Timeout bounds the wait until the release is ready, which an install
	// always does; 0 leaves it to the servers.
	Timeout time.Duration
	structs.WriteRequest
}

// RiggerInstallResponse is the plan of an install, and the revision an
// apply made.
type RiggerInstallResponse struct {
	Action   string
	Module   host.ModuleRef
	Changes  []host.Change
	Revision uint64
	// Origin is where a module of a registry came from and what verified
	// it; nil for a local module.
	Origin   *rigger.ModuleOrigin
	Warnings []string
	structs.WriteMeta
}

// RiggerDetailRequest asks for a version of a module of a registry.
type RiggerDetailRequest struct {
	RiggerModuleRequest
	structs.QueryOptions
}

// RiggerDetailResponse is what an install form needs of a version.
type RiggerDetailResponse struct {
	Origin     *rigger.ModuleOrigin
	Metadata   module.Metadata
	Readme     string
	Variables  []module.Variable
	Tabs       []module.Tab
	FormSchema map[string]any
	// Versions are the versions of the module in the registry, the newest
	// first.
	Versions []string
	// Meets reports whether the cluster meets the requirements of the
	// version; Unmet lists what it lacks.
	Meets bool
	Unmet []string
	// Icon reports whether the version has an icon (Rigger.Icon).
	Icon bool
	structs.QueryMeta
}

// RiggerIconResponse is the icon of a version: a PNG of 64x64 pixels.
type RiggerIconResponse struct {
	Icon []byte
	structs.QueryMeta
}

// Install installs or upgrades one release of a module: it fetches and
// verifies a module of a synced registry (or loads a local one), evaluates
// it with the values and plans or applies its release with the caller's
// token. It runs on the leader.
func (r *Rigger) Install(args *RiggerInstallRequest, reply *RiggerInstallResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.Install", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.Install", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	return r.srv.riggerInstall(args, reply, r.ctx)
}

// Detail returns what an install form needs of a version of a module of a
// registry: its metadata, README, variables and form, the versions of the
// module, and whether the cluster meets its requirements. It needs read on
// the kind of registries.
func (r *Rigger) Detail(args *RiggerDetailRequest, reply *RiggerDetailResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.Detail", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.Detail", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	return r.srv.riggerDetail(args, reply)
}

// Icon returns the icon of a version of a module of a registry. It needs
// read on the kind of registries.
func (r *Rigger) Icon(args *RiggerDetailRequest, reply *RiggerIconResponse) (retErr error) {
	defer r.srv.recoverRigger("Rigger.Icon", &retErr)

	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.Icon", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	src, err := r.srv.newRiggerSources(args)
	if err != nil {
		return err
	}

	eng, err := r.srv.riggerFetchEngine(src)
	if err != nil {
		return err
	}

	m, _, err := r.srv.riggerRegistryModule(ctx, eng, src, args.RiggerModuleRequest)
	if err != nil {
		return err
	}

	icon, err := moduleFile(m, "icon.png")
	if errors.Is(err, fs.ErrNotExist) {
		return structs.NewErrRPCCodedf(http.StatusNotFound, "%s %s has no icon", args.Module, m.Metadata().Version)
	}

	if err != nil {
		return err
	}

	reply.Icon = icon
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// riggerInstall installs a module (Rigger.Install), on the leader.
func (s *Server) riggerInstall(args *RiggerInstallRequest, reply *RiggerInstallResponse, rpcCtx *RPCContext) error {
	namespace := args.RequestNamespace()
	local := len(args.Files) > 0
	ofServers := !local && args.Registry == "" && strings.HasPrefix(args.Module, riggerBuiltinPrefix)

	if err := s.authorizeRiggerInstall(args, namespace, !local && !ofServers); err != nil {
		return err
	}

	if err := structs.ValidateReleaseName(args.Release); err != nil {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid release name: %v", err)
	}

	if args.Timeout < 0 {
		return structs.NewErrRPCCodedf(http.StatusBadRequest, "the timeout %s is negative", args.Timeout)
	}

	if !local && !ofServers && args.Registry == "" {
		return structs.NewErrRPCCodedf(http.StatusBadRequest,
			"the module %q names no registry: a module of a registry of the cluster is Registry and Module, "+
				"a module built into the servers is Module %s<name> alone, a local module is sent as Files",
			args.Module, riggerBuiltinPrefix)
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	src, err := s.newRiggerSources(args)
	if err != nil {
		return err
	}

	// A module of a registry is fetched and verified by the server: its
	// release is admitted as rendered by the server. A local one is a
	// client's.
	// A module of a registry records the registry of the cluster it came
	// from: what an upgrade in the UI takes it from again.
	eng, err := s.riggerServerEngine(args.AuthToken, namespace, args.Registry, !local, rpcCtx, src)
	if err != nil {
		return err
	}

	in := rigger.Install{Release: args.Release, Namespace: namespace, Region: s.Region()}

	var (
		m     *rigger.Module
		diags hcl.Diagnostics
	)

	if local {
		if args.Registry != "" || args.Module != "" {
			return structs.NewErrRPCCodedf(http.StatusBadRequest, "a local module is sent as files, without a registry")
		}

		m, diags = eng.LoadModuleFiles(ctx, args.Files)
		if diags.HasErrors() {
			return riggerDiagsError(diags)
		}

		in.Source = riggerLocalSource
	} else if ofServers {
		if m, err = riggerBuiltinModule(ctx, eng, src, args.RiggerModuleRequest); err != nil {
			return err
		}
	} else {
		if m, _, err = s.riggerRegistryModule(ctx, eng, src, args.RiggerModuleRequest); err != nil {
			return err
		}
	}

	if in.Values, err = riggerValues(args.Values, args.ValuesJSON); err != nil {
		return err
	}

	// As the rigger program installs: the settings are in the digest of the
	// release, and an install succeeds when its release is ready.
	rp, diags := eng.PlanInstall(ctx, m, in, host.Settings{Wait: true, Timeout: args.Timeout})
	if diags.HasErrors() {
		return riggerDiagsError(diags)
	}

	reply.Action, reply.Module, reply.Changes, reply.Origin = string(rp.Action), rp.Module, rp.Changes, m.Origin()
	reply.Warnings = riggerWarnings(m, diags)
	reply.Revision = rp.Revision

	if rp.TakesOver != "" {
		reply.Warnings = append(reply.Warnings,
			fmt.Sprintf("the release %s belongs to %s: an apply takes it over only with adopt (the flag -adopt)", args.Release, rp.TakesOver))
	}

	if !args.Plan {
		outcome, err := eng.ApplyInstall(ctx, rp, rigger.ApplyOptions{Adopt: args.Adopt})
		if errors.Is(err, rigger.ErrAdoptNeeded) {
			return structs.NewErrRPCCodedf(http.StatusConflict, "%v", err)
		}

		if err != nil {
			return err
		}

		reply.Revision = outcome.Revision
	}

	reply.Index = max(1, s.raft.AppliedIndex())

	return nil
}

// authorizeRiggerInstall checks what an install needs before anything is
// fetched: write on releases in the namespace, and read on the kind of
// registries for a module of a registry (ofRegistry); a local module and
// one built into the servers need no more. The release API checks the
// write again, with everything else of an apply.
func (s *Server) authorizeRiggerInstall(args *RiggerInstallRequest, namespace string, ofRegistry bool) error {
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return err
	}

	if aclObj == nil {
		return nil
	}

	if !aclObj.AllowObjectOperation(namespace, structs.ReleaseKindRef, acl.ObjectCapabilityWrite) {
		return structs.ErrPermissionDenied
	}

	if ofRegistry && !aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, structs.RiggerRegistryKindRef, acl.ObjectCapabilityRead) {
		return structs.ErrPermissionDenied
	}

	return nil
}

// riggerServerEngine returns an engine with the hosts of this server and the
// sources of the request. The releases are applied as rendered by the
// server when serverRendered is set and an index the servers synced vouches
// for all that was fetched for them.
func (s *Server) riggerServerEngine(token, namespace, registry string, serverRendered bool, rpcCtx *RPCContext,
	src *riggerSources, opts ...rigger.Option,
) (*rigger.Engine, error) {
	// The bundles imported into the cluster are sources: unpacked into the
	// cache of this server first. Without their list none is one.
	ctx, cancel := context.WithTimeout(context.Background(), riggerBundleTimeout)
	defer cancel()

	bundled, err := src.registries.unpackBundles(ctx)
	if err != nil {
		s.logger.Named("rigger").Warn("the bundles of the cluster could not be unpacked", "error", err)
	}

	src.bundled = bundled

	h := &riggerHosts{srv: s, token: token, namespace: namespace, registry: registry}
	h.apply = func(req *structs.ReleaseApplyRequest, resp *structs.ReleaseApplyResponse) error {
		return NewReleaseEndpoint(s, rpcCtx).applyInternal(req, resp, serverRendered && src.indexed(), "")
	}

	return s.riggerEngine(h, append([]rigger.Option{rigger.WithFetcher(src)}, opts...)...)
}

// riggerFetchEngine returns an engine that only fetches and loads modules,
// with the fetcher of the request and the decoders of the hosts and no
// cluster: what Rigger.Detail and Rigger.Icon need.
func (s *Server) riggerFetchEngine(fetcher source.Fetcher) (*rigger.Engine, error) {
	return rigger.New(
		rigger.WithLogger(slog.New(&hclogHandler{logger: s.logger.Named("rigger")})),
		rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithKinds(norcaapi.Kinds()...),
		rigger.WithFetcher(fetcher),
	)
}

// riggerRequestFetcher returns the fetcher of one request: the cache and
// the credentials of the registries, and an index of the global registry
// of its own, empty until the request reads one (useGlobal). A request
// that runs beside it does not move it.
func (s *Server) riggerRequestFetcher() (*fetch.Fetcher, error) {
	r, err := s.riggerRegistriesOf()
	if err != nil {
		return nil, err
	}

	dir, err := r.noGlobalDir()
	if err != nil {
		return nil, err
	}

	return r.fetcher.WithGlobalRegistry(dir), nil
}

// riggerRegistryModule fetches and verifies the version of a module of a
// registry that the request names: the newest the last good sync of the
// registry offers that meets the constraint.
func (s *Server) riggerRegistryModule(ctx context.Context, eng *rigger.Engine, src *riggerSources,
	req RiggerModuleRequest,
) (*rigger.Module, []string, error) {
	reg, err := s.syncedRegistry(req.Registry)
	if err != nil {
		return nil, nil, err
	}

	if ctx, err = src.use(ctx, reg.Status.Source); err != nil {
		return nil, nil, err
	}

	idx, err := src.registries.read(ctx, reg.Spec.Type, reg.Status.Source, reg.Status.Commit)
	if err != nil {
		return nil, nil, err
	}

	items := idx.items

	ref := catalog.Ref{Kind: index.KindModule, Name: req.Module}
	if reg.Spec.Type == riggerRegistryTypeGlobal {
		ns, name, ok := strings.Cut(req.Module, "/")
		if !ok {
			return nil, nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "a module of the global registry is <namespace>/<name>, not %q", req.Module)
		}

		ref.Namespace, ref.Name = ns, name
	}

	versions, err := itemVersions(items, ref)
	if err != nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s of the registry %q: %v", ref, reg.Name, err)
	}

	v, err := newestMeeting(versions, req.Version)
	if err != nil {
		return nil, nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s of the registry %q at its last sync: %v (rigger registry sync %s fetches newer versions)", ref, reg.Name, err, reg.Name)
	}

	// The request names its registry: the rules are that one's.
	if err := src.only(ctx, reg); err != nil {
		return nil, nil, err
	}

	sreq, keys, err := s.moduleSourceRequest(ctx, reg, ref, v)
	if err != nil {
		return nil, nil, err
	}

	m, diags := eng.FetchModule(ctx, sreq, keys)
	if diags.HasErrors() {
		if err := src.refusal(); err != nil {
			return nil, nil, err
		}

		return nil, nil, riggerDiagsError(diags)
	}

	return m, versions, nil
}

// riggerBuiltinPrefix starts the address of a module built into the servers
// (and into the rigger program): builtin/<name>.
const riggerBuiltinPrefix = "builtin/"

// riggerBuiltinModule fetches a module built into the servers: the one
// version they have, which must meet the constraint of the request. The
// program is its source, so it needs no signature and no registry, and
// what it renders counts as rendered by the server (riggerSources.indexed).
func riggerBuiltinModule(ctx context.Context, eng *rigger.Engine, src *riggerSources, req RiggerModuleRequest) (*rigger.Module, error) {
	sreq, err := source.Parse(req.Module, nil)
	if err != nil || sreq.Kind != source.KindBuiltin {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%q is not the address of a built-in module, %s<name>", req.Module, riggerBuiltinPrefix)
	}

	if _, err := fs.Stat(builtin.FS(), path.Join(sreq.Path, "metadata.hcl")); err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "the servers have no built-in module %s", req.Module)
	}

	m, diags := eng.FetchNewestModule(ctx, req.Module, req.Version)
	if diags.HasErrors() {
		if err := src.refusal(); err != nil {
			return nil, err
		}

		return nil, riggerDiagsError(diags)
	}

	return m, nil
}

// syncedRegistry returns a registry with a good sync.
func (s *Server) syncedRegistry(name string) (*RiggerRegistry, error) {
	obj, err := s.State().CustomObjectByName(nil, structs.RiggerObjectGroup, structs.RiggerRegistryKind, "", name)
	if err != nil {
		return nil, err
	}

	if obj == nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "%s %q not found", structs.RiggerRegistryKindRef, name)
	}

	reg, err := riggerRegistryFromObject(obj)
	if err != nil {
		return nil, err
	}

	if reg.Status.Commit == "" {
		return nil, errRiggerRegistryNotSynced(name)
	}

	return reg, nil
}

// moduleSourceRequest is the request of the fetcher for a version of a
// module of a registry, and the keys that must sign it: the global registry
// read at the commit of its sync, a registry repository at the tag of the
// version in the history of that commit.
func (s *Server) moduleSourceRequest(ctx context.Context, reg *RiggerRegistry,
	ref catalog.Ref, v string,
) (source.Request, []string, error) {
	r, err := s.riggerRegistriesOf()
	if err != nil {
		return source.Request{}, nil, err
	}

	if reg.Spec.Type == riggerRegistryTypeGlobal {
		return source.Request{
			Source: ref.Namespace + "/" + ref.Name, Kind: source.KindGlobal,
			Namespace: ref.Namespace, Name: ref.Name, Path: ".", Version: v,
		}, nil, nil
	}

	prefix := ref.Name + "/v"

	tags, err := r.fetcher.TagsAt(ctx, reg.Status.Source, reg.Status.Commit, prefix)
	if err != nil {
		return source.Request{}, nil, err
	}

	for _, tag := range tags {
		if tag.Name == prefix+v {
			return source.Request{
				Source: reg.Name + "/" + ref.Name, Kind: source.KindGit, Registry: reg.Name,
				Repository: reg.Status.Source, Path: path.Join("modules", ref.Name), TagPrefix: prefix,
				Version: v, Ref: tag.Name, Commit: tag.Commit,
			}, reg.Spec.TrustedKeys, nil
		}
	}

	return source.Request{}, nil, fmt.Errorf("the registry %q has no tag %s%s at %s", reg.Name, prefix, v, reg.Status.Commit)
}

// itemVersions returns the versions of a module or stack of a registry, the
// newest first.
func itemVersions(items []catalog.Item, ref catalog.Ref) ([]string, error) {
	for _, item := range items {
		if item.Ref == ref {
			if len(item.Versions) == 0 {
				return nil, errors.New("no version is tagged")
			}

			return item.Versions, nil
		}
	}

	return nil, errors.New("not in the registry")
}

// newestMeeting returns the newest of versions (the newest first) that
// meets the constraint; an empty constraint takes the newest.
func newestMeeting(versions []string, constraint string) (string, error) {
	if constraint == "" {
		return versions[0], nil
	}

	c, err := version.ParseConstraint(constraint)
	if err != nil {
		return "", err
	}

	for _, v := range versions {
		parsed, err := version.Parse(v)
		if err == nil && c.Check(parsed) {
			return v, nil
		}
	}

	return "", fmt.Errorf("no version meets %q", constraint)
}

// riggerDetail answers Rigger.Detail.
func (s *Server) riggerDetail(args *RiggerDetailRequest, reply *RiggerDetailResponse) error {
	ctx, cancel := context.WithTimeout(context.Background(), riggerInstallTimeout)
	defer cancel()

	src, err := s.newRiggerSources(args)
	if err != nil {
		return err
	}

	eng, err := s.riggerFetchEngine(src)
	if err != nil {
		return err
	}

	m, versions, err := s.riggerRegistryModule(ctx, eng, src, args.RiggerModuleRequest)
	if err != nil {
		return err
	}

	md := m.Metadata()
	reply.Origin, reply.Metadata, reply.Versions = m.Origin(), md, versions
	reply.Variables, reply.Tabs, reply.FormSchema = m.Variables(), m.Tabs(), m.FormSchema()

	if readme, err := moduleFile(m, "README.md"); err == nil {
		reply.Readme = string(readme)
	}

	if _, err := moduleFile(m, "icon.png"); err == nil {
		reply.Icon = true
	}

	reply.Meets, reply.Unmet = s.evaluatePackCompatibility(&structs.ReleasePackRequires{
		Norca: md.Requires.Norca, Features: md.Requires.Features,
	})
	reply.Index = max(1, s.raft.AppliedIndex())

	return nil
}

// moduleFile reads a file of the directory of a module.
func moduleFile(m *rigger.Module, name string) ([]byte, error) {
	root, _ := m.Files()

	return fs.ReadFile(root, path.Join(m.Dir(), name))
}

// riggerWarnings are the warnings of an install: those of the origin of the
// module and those of its evaluation.
func riggerWarnings(m *rigger.Module, diags hcl.Diagnostics) []string {
	var out []string

	if o := m.Origin(); o != nil {
		out = append(out, o.Warnings...)
	}

	for _, d := range diags {
		if d.Severity == hcl.DiagWarning {
			out = append(out, strings.TrimSpace(d.Summary+": "+d.Detail))
		}
	}

	return out
}

// riggerDiagsError is the error of the diagnostics of a module, a request
// the client can correct.
func riggerDiagsError(diags hcl.Diagnostics) error {
	return structs.NewErrRPCCodedf(http.StatusBadRequest, "%s", diags.Error())
}
