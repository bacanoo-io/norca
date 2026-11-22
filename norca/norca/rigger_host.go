// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/go-hclog"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	norcaversion "github.com/bacanoo-io/norca/norca/version"
	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/cli/norcaapi"
	"github.com/bacanoo-io/norca/rigger/host"
)

// The hosts of Rigger in the server (design/rig-integration.md 4.3): an
// engine of the library built from the server's own parts. Every operation
// goes through the endpoints of the server with the token of the caller, as
// it would over HTTP, so the ACL checks, the release lock and fencing,
// admission and the forwarding to the leader all hold: the release API
// (Release.*), the secret store (Secret.*) and the namespaces. The decoders
// are those of the rigger program (cli/norcaapi: Norca's jobspec2 and
// resourcespec), and so is the mapping between the engine and the release
// API; the requests of the API become requests of the servers as a pack the
// server renders does (Config.ReleaseFromRendered).

// riggerHosts are the ports of one engine: the caller's token and the
// namespace of the secrets of a stack.
type riggerHosts struct {
	srv       *Server
	token     string
	namespace string

	// apply applies a release request (a dry run too); Release.Apply in
	// the process unless set.
	apply func(*structs.ReleaseApplyRequest, *structs.ReleaseApplyResponse) error
}

// RiggerEngine returns an engine of Rigger with the hosts of this server:
// the token authorizes every operation through the endpoints of the server,
// and namespace is where the secrets of a stack live. The local data of
// the engine is kept in <data_dir>/rigger/store. It has no fetcher of its
// own: a server fetches through the sources of a request only
// (rigger_sources.go).
func (s *Server) RiggerEngine(token, namespace string, opts ...rigger.Option) (*rigger.Engine, error) {
	h := &riggerHosts{srv: s, token: token, namespace: namespace}

	return s.riggerEngine(h, opts...)
}

func (s *Server) riggerEngine(h *riggerHosts, opts ...rigger.Option) (*rigger.Engine, error) {
	if s.config.ReleaseFromRendered == nil {
		return nil, errors.New("the release requests of Rigger can't be converted on this server")
	}

	dir, err := s.riggerDir()
	if err != nil {
		return nil, err
	}

	base := []rigger.Option{
		rigger.WithLogger(slog.New(&hclogHandler{logger: s.logger.Named("rigger")})),
		rigger.WithJobDecoder(norcaapi.JobDecoder{}),
		rigger.WithResourceDecoder(norcaapi.ResourceDecoder{}),
		rigger.WithCluster(riggerCluster{h}),
		rigger.WithReleases(riggerReleases{h}),
		rigger.WithSecrets(riggerSecrets{h}),
		rigger.WithStore(riggerStore{dir: filepath.Join(dir, "store")}),
	}

	return rigger.New(append(base, opts...)...)
}

// riggerDir returns the directory of the data of Rigger on this server:
// <data_dir>/rigger, or for a server without a data directory (a dev agent,
// tests) a temporary directory made once. Never a path relative to the
// working directory of the agent.
func (s *Server) riggerDir() (string, error) {
	s.riggerDirOnce.Do(func() {
		if s.config.DataDir != "" {
			s.riggerDirPath = filepath.Join(s.config.DataDir, "rigger")

			return
		}

		s.riggerDirPath, s.riggerDirErr = os.MkdirTemp("", "norca-rigger-")
	})

	return s.riggerDirPath, s.riggerDirErr
}

func (h *riggerHosts) write(namespace string) structs.WriteRequest {
	return structs.WriteRequest{Region: h.srv.Region(), Namespace: namespace, AuthToken: h.token}
}

func (h *riggerHosts) query(namespace string) structs.QueryOptions {
	return structs.QueryOptions{Region: h.srv.Region(), Namespace: namespace, AuthToken: h.token}
}

// asAPI converts a value of the servers into its form in the api module,
// through JSON: the HTTP API sends the structs as JSON and the api module
// reads them so.
func asAPI[T any](v any) (*T, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}

	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// notFoundErr maps a 404 of an endpoint onto host.ErrNotFound.
func notFoundErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "404") {
		return errors.Join(host.ErrNotFound, err)
	}

	return err
}

// riggerCluster is host.Cluster: the version, region and features of this
// server, and the namespaces the token may see.
type riggerCluster struct{ h *riggerHosts }

func (c riggerCluster) Info(context.Context) (host.ClusterInfo, error) {
	return host.ClusterInfo{
		Version:  norcaversion.NorcaVersion,
		Region:   c.h.srv.Region(),
		Features: slices.Clone(releaseFeatures),
	}, nil
}

func (c riggerCluster) Namespaces(context.Context) ([]string, error) {
	var resp structs.NamespaceListResponse
	if err := c.h.srv.RPC("Namespace.ListNamespaces", &structs.NamespaceListRequest{QueryOptions: c.h.query("")}, &resp); err != nil {
		return nil, err
	}

	names := make([]string, 0, len(resp.Namespaces))
	for _, ns := range resp.Namespaces {
		names = append(names, ns.Name)
	}

	return names, nil
}

// riggerReleases is host.Releases over the release endpoints.
type riggerReleases struct{ h *riggerHosts }

// request converts a request of the engine into one of the servers.
func (r riggerReleases) request(req host.ApplyRequest, dryRun bool) (*structs.ReleaseApplyRequest, error) {
	areq, err := norcaapi.ApplyRequest(req)
	if err != nil {
		return nil, err
	}

	sreq, err := r.h.srv.config.ReleaseFromRendered(req.Release.Name, areq, req.Release.Namespace)
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	sreq.DryRun = dryRun
	sreq.WriteRequest = r.h.write(req.Release.Namespace)

	return sreq, nil
}

func (r riggerReleases) applyRPC(req *structs.ReleaseApplyRequest) (*structs.ReleaseApplyResponse, error) {
	var resp structs.ReleaseApplyResponse

	apply := r.h.apply
	if apply == nil {
		apply = func(args *structs.ReleaseApplyRequest, reply *structs.ReleaseApplyResponse) error {
			return r.h.srv.RPC("Release.Apply", args, reply)
		}
	}

	return &resp, apply(req, &resp)
}

func (r riggerReleases) Current(ctx context.Context, ref host.ReleaseRef) (host.Revision, error) {
	var rel structs.SingleReleaseResponse
	if err := r.h.srv.RPC("Release.GetRelease", &structs.ReleaseSpecificRequest{Name: ref.Name, QueryOptions: r.h.query(ref.Namespace)}, &rel); err != nil {
		return host.Revision{}, notFoundErr(err)
	}

	if rel.Release == nil || rel.Release.Spec == nil || rel.Release.Spec.Revision == 0 {
		return host.Revision{}, fmt.Errorf("%w: release %s/%s", host.ErrNotFound, ref.Namespace, ref.Name)
	}

	return r.revision(ref, rel.Release.Spec.Revision)
}

func (r riggerReleases) revision(ref host.ReleaseRef, n uint64) (host.Revision, error) {
	var resp structs.SingleReleaseRevisionResponse

	err := r.h.srv.RPC("Release.GetRevision", &structs.ReleaseRevisionRequest{
		Name: ref.Name, Revision: n, QueryOptions: r.h.query(ref.Namespace),
	}, &resp)
	if err != nil {
		return host.Revision{}, notFoundErr(err)
	}

	if resp.Revision == nil {
		return host.Revision{}, fmt.Errorf("%w: revision %d of %s/%s", host.ErrNotFound, n, ref.Namespace, ref.Name)
	}

	rev, err := asAPI[api.ReleaseRevision](resp.Revision)
	if err != nil {
		return host.Revision{}, err
	}

	return norcaapi.RevisionOf(ref, rev), nil
}

func (r riggerReleases) History(ctx context.Context, ref host.ReleaseRef) ([]host.Revision, error) {
	var resp structs.ReleaseHistoryResponse
	if err := r.h.srv.RPC("Release.History", &structs.ReleaseSpecificRequest{Name: ref.Name, QueryOptions: r.h.query(ref.Namespace)}, &resp); err != nil {
		return nil, notFoundErr(err)
	}

	revs, err := asAPI[[]*api.ReleaseRevision](resp.Revisions)
	if err != nil {
		return nil, err
	}

	out := make([]host.Revision, 0, len(*revs))
	for _, rev := range *revs {
		out = append(out, norcaapi.RevisionOf(ref, rev))
	}

	slices.SortFunc(out, func(a, b host.Revision) int { return cmp.Compare(b.Number, a.Number) })

	return out, nil
}

func (r riggerReleases) Owned(ctx context.Context, owner string) ([]host.Revision, error) {
	// The releases of the stack in every namespace the token may read.
	req := &structs.ReleaseListRequest{
		Owner:        structs.ReleaseOwnerKindStack + "/" + owner,
		QueryOptions: r.h.query(structs.AllNamespacesSentinel),
	}

	var resp structs.ReleaseListResponse
	if err := r.h.srv.RPC("Release.List", req, &resp); err != nil {
		return nil, err
	}

	rels, err := asAPI[[]*api.Release](resp.Releases)
	if err != nil {
		return nil, err
	}

	return norcaapi.OwnedOf(*rels, owner), nil
}

func (r riggerReleases) Plan(ctx context.Context, req host.ApplyRequest) (host.Plan, error) {
	sreq, err := r.request(req, true)
	if err != nil {
		return host.Plan{}, err
	}

	resp, err := r.applyRPC(sreq)
	if err != nil {
		return host.Plan{}, err
	}

	aresp, err := asAPI[api.ReleaseApplyResponse](resp)
	if err != nil {
		return host.Plan{}, err
	}

	return norcaapi.PlanOf(req.Release, aresp), nil
}

func (r riggerReleases) Drift(ctx context.Context, ref host.ReleaseRef) (host.Drift, error) {
	var resp structs.ReleaseDriftResponse
	if err := r.h.srv.RPC("Release.Drift", &structs.ReleaseDriftRequest{Name: ref.Name, WriteRequest: r.h.write(ref.Namespace)}, &resp); err != nil {
		return host.Drift{}, notFoundErr(err)
	}

	aresp, err := asAPI[api.ReleaseDriftResponse](&resp)
	if err != nil {
		return host.Drift{}, err
	}

	return norcaapi.DriftOf(ref, aresp), nil
}

func (r riggerReleases) Apply(ctx context.Context, req host.ApplyRequest) (host.Revision, error) {
	sreq, err := r.request(req, false)
	if err != nil {
		return host.Revision{}, err
	}

	resp, err := r.applyRPC(sreq)
	if err != nil {
		return host.Revision{}, err
	}

	rev := host.Revision{Release: req.Release, Number: resp.Revision, Module: req.Module, Digest: req.Digest, Owner: req.Owner}
	if resp.Release != nil && resp.Release.Status != nil {
		rev.Status = resp.Release.Status.Phase
	}

	return rev, nil
}

func (r riggerReleases) Rollback(ctx context.Context, ref host.ReleaseRef, number uint64) (host.Revision, error) {
	var resp structs.ReleaseApplyResponse

	err := r.h.srv.RPC("Release.Rollback", &structs.ReleaseRollbackRequest{
		Name: ref.Name, Revision: number, Wait: true, WriteRequest: r.h.write(ref.Namespace),
	}, &resp)
	if err != nil {
		return host.Revision{}, notFoundErr(err)
	}

	return r.revision(ref, resp.Revision)
}

func (r riggerReleases) Uninstall(ctx context.Context, ref host.ReleaseRef) error {
	var resp structs.ReleaseDeleteResponse

	return notFoundErr(r.h.srv.RPC("Release.Delete", &structs.ReleaseDeleteRequest{
		Name: ref.Name, Propagation: structs.ReleasePropagationBackground, WriteRequest: r.h.write(ref.Namespace),
	}, &resp))
}

// riggerSecrets is host.Secrets over the secret store of the namespace of
// the engine.
type riggerSecrets struct{ h *riggerHosts }

func (s riggerSecrets) Ensure(ctx context.Context, secret host.Secret) (bool, error) {
	items := make(structs.SecretItems, len(secret.Items))
	for k, v := range secret.Items {
		items[k] = v.Reveal()
	}

	req := &structs.SecretUpsertRequest{Path: secret.Path, Items: items, WriteRequest: s.h.write(s.h.namespace)}
	if !secret.Overwrite {
		zero := uint64(0)
		req.CheckIndex = &zero
	}

	var resp structs.SecretWriteResponse

	err := s.h.srv.RPC("Secret.Upsert", req, &resp)
	if err != nil && !secret.Overwrite && strings.Contains(err.Error(), "409") {
		// Made meanwhile by another: left as it is.
		return false, nil
	}

	return err == nil, err
}

func (s riggerSecrets) Read(ctx context.Context, path string) (map[string]host.Sensitive, error) {
	var resp structs.SecretReadResponse
	if err := s.h.srv.RPC("Secret.Read", &structs.SecretReadRequest{Path: path, QueryOptions: s.h.query(s.h.namespace)}, &resp); err != nil {
		return nil, notFoundErr(err)
	}

	if resp.Secret == nil {
		return nil, fmt.Errorf("%w: secret %s", host.ErrNotFound, path)
	}

	out := make(map[string]host.Sensitive, len(resp.Items))
	for k, v := range resp.Items {
		out[k] = host.NewSensitive(v)
	}

	return out, nil
}

// riggerStore is host.Store in a directory of the data dir of the server.
type riggerStore struct{ dir string }

func (s riggerStore) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(key))
	if key == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid key %q", key)
	}

	return filepath.Join(s.dir, clean), nil
}

func (s riggerStore) Get(_ context.Context, key string) ([]byte, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(host.ErrNotFound, err)
	}

	return data, err
}

func (s riggerStore) Put(_ context.Context, key string, data []byte) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-")
	if err != nil {
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return err
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())

		return err
	}

	return os.Rename(tmp.Name(), p)
}

func (s riggerStore) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}

	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

// hclogHandler writes the records of Rigger's slog logger into the hclog
// logger of the server: the level, the message, and the attributes as
// key/value pairs (groups as dotted keys).
type hclogHandler struct {
	logger hclog.Logger
	attrs  []any
	group  string
}

func (h *hclogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.logger.GetLevel() <= hclogLevel(level)
}

func (h *hclogHandler) Handle(_ context.Context, r slog.Record) error {
	args := slices.Clone(h.attrs)

	r.Attrs(func(a slog.Attr) bool {
		args = append(args, h.key(a.Key), a.Value.Resolve().Any())

		return true
	})

	h.logger.Log(hclogLevel(r.Level), r.Message, args...)

	return nil
}

func (h *hclogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = slices.Clone(h.attrs)

	for _, a := range attrs {
		next.attrs = append(next.attrs, h.key(a.Key), a.Value.Resolve().Any())
	}

	return &next
}

func (h *hclogHandler) WithGroup(name string) slog.Handler {
	next := *h
	next.group = h.key(name)

	return &next
}

func (h *hclogHandler) key(k string) string {
	if h.group == "" {
		return k
	}

	return h.group + "." + k
}

func hclogLevel(l slog.Level) hclog.Level {
	switch {
	case l >= slog.LevelError:
		return hclog.Error
	case l >= slog.LevelWarn:
		return hclog.Warn
	case l >= slog.LevelInfo:
		return hclog.Info
	default:
		return hclog.Debug
	}
}
