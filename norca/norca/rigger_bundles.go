// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/bacanoo-io/norca/norca/acl"
	"github.com/bacanoo-io/norca/norca/norca/bus"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/rigger/bundle"
)

// Bundles of Rigger imported into the cluster (design/rig-integration.md
// 4.6c): the modules of a stack from their git sources, for a cluster
// without a network. They are kept in a JetStream object store of the bus,
// replicated between the servers; the server checks the digests of a
// bundle when it is imported, and the leader unpacks into its cache what
// its cache lacks before it evaluates anything, so a bundle outlives a
// change of leader. The fetcher of Rigger takes a tree of a bundle when the
// source of a locked version can't be reached.

const (
	// riggerBundleMaxBytes bounds one bundle as it is sent, and
	// riggerBundleMaxUnpacked what it may unpack to.
	riggerBundleMaxBytes    = 64 << 20
	riggerBundleMaxUnpacked = 512 << 20

	// riggerBundleTimeout bounds an import, a list, a delete or the unpacking
	// of the bundles before an evaluation.
	riggerBundleTimeout = 5 * time.Minute

	riggerBundleNamePrefix = "bundle-"
)

// RiggerBundle is a bundle kept in the cluster.
type RiggerBundle struct {
	// Name is "bundle-" and the start of the SHA-256 of the archive.
	Name  string
	Stack string
	// Modules are "source version" of each module of the bundle.
	Modules  []string
	Size     uint64
	Imported time.Time
}

// RiggerBundleImportRequest imports a bundle (the archive of rigger bundle
// create).
type RiggerBundleImportRequest struct {
	Bundle []byte
	structs.WriteRequest
}

// RiggerBundleResponse is a bundle that was imported.
type RiggerBundleResponse struct {
	Bundle *RiggerBundle
	structs.WriteMeta
}

// RiggerBundleNameRequest names a bundle.
type RiggerBundleNameRequest struct {
	Name string
	structs.WriteRequest
}

// RiggerBundleListRequest lists the bundles.
type RiggerBundleListRequest struct {
	structs.QueryOptions
}

// RiggerBundleListResponse are the bundles.
type RiggerBundleListResponse struct {
	Bundles []*RiggerBundle
	structs.QueryMeta
}

// ImportBundle checks a bundle and keeps it in the object store of the bus.
// It needs write on the kind of registries: a bundle is a source of modules
// for the whole cluster.
func (r *Rigger) ImportBundle(args *RiggerBundleImportRequest, reply *RiggerBundleResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.ImportBundle", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityWrite); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerBundleTimeout)
	defer cancel()

	b, err := r.srv.importRiggerBundle(ctx, args.Bundle)
	if err != nil {
		return err
	}

	reply.Bundle = b
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// ListBundles lists the bundles kept in the cluster. It needs read on the
// kind of registries.
func (r *Rigger) ListBundles(args *RiggerBundleListRequest, reply *RiggerBundleListResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.ListBundles", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityRead); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerBundleTimeout)
	defer cancel()

	bundles, err := r.srv.listRiggerBundles(ctx)
	if err != nil {
		return err
	}

	reply.Bundles = bundles
	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// DeleteBundle removes a bundle from the cluster. The trees it brought stay
// in the caches of the servers, where nothing uses them unless a lock file
// names their commits. It needs write on the kind of registries.
func (r *Rigger) DeleteBundle(args *RiggerBundleNameRequest, reply *structs.GenericResponse) error {
	authErr := r.srv.Authenticate(r.ctx, args)
	if done, err := r.srv.forward("Rigger.DeleteBundle", args, args, reply); done {
		return err
	}

	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	if err := r.authorize(args, acl.ObjectCapabilityWrite); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), riggerBundleTimeout)
	defer cancel()

	err := r.srv.withRiggerBundles(ctx, func(obs jetstream.ObjectStore) error {
		// A deleted object stays as a mark, which a second delete takes:
		// the bundle must be there.
		if _, err := obs.GetInfo(ctx, args.Name); err != nil {
			if errors.Is(err, jetstream.ErrObjectNotFound) {
				return structs.NewErrRPCCodedf(http.StatusNotFound, "bundle %q not found", args.Name)
			}

			return err
		}

		return obs.Delete(ctx, args.Name)
	})
	if err != nil {
		return err
	}

	reply.Index = max(1, r.srv.raft.AppliedIndex())

	return nil
}

// keepRiggerBundleStore creates the object store of the bundles and keeps
// its settings. It runs on the leader.
func (s *Server) keepRiggerBundleStore(ctx context.Context, js jetstream.JetStream) error {
	if s.config.Bus.RiggerBundlesBytes() <= 0 {
		return nil
	}

	check := time.NewTicker(busStreamCheckInterval)
	defer check.Stop()

	for {
		if _, err := s.ensureRiggerBundleStore(ctx, js); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-check.C:
		}
	}
}

func (s *Server) ensureRiggerBundleStore(ctx context.Context, js jetstream.JetStream) (jetstream.ObjectStore, error) {
	return js.CreateOrUpdateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket:      bus.RiggerBundleBucket(s.config.Region),
		Description: "Norca Rigger bundles of region " + s.config.Region,
		MaxBytes:    s.config.Bus.RiggerBundlesBytes(),
		Storage:     jetstream.FileStorage,
		Replicas:    s.busReplicas(),
	})
}

// withRiggerBundles runs fn with the object store of the bundles, made if
// the leader has not yet.
func (s *Server) withRiggerBundles(ctx context.Context, fn func(jetstream.ObjectStore) error) error {
	if s.bus == nil || !s.bus.JetStreamReady() {
		return structs.NewErrRPCCodedf(http.StatusServiceUnavailable, "bundles are kept on the system bus, which is not ready on this server")
	}

	if s.config.Bus.RiggerBundlesBytes() <= 0 {
		return structs.NewErrRPCCodedf(http.StatusServiceUnavailable,
			"the other streams of the bus leave no room for bundles: raise bus.max_storage or set bus.rigger_bundles_max_bytes")
	}

	nc, err := s.bus.Connect()
	if err != nil {
		return err
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}

	obs, err := s.ensureRiggerBundleStore(ctx, js)
	if err != nil {
		return err
	}

	return fn(obs)
}

// importRiggerBundle checks a bundle (each module of it unpacks to the
// digest its manifest says) and keeps it, named by its SHA-256.
func (s *Server) importRiggerBundle(ctx context.Context, data []byte) (*RiggerBundle, error) {
	if len(data) > riggerBundleMaxBytes {
		return nil, structs.NewErrRPCCodedf(http.StatusRequestEntityTooLarge, "the bundle is %d bytes, more than %d", len(data), riggerBundleMaxBytes)
	}

	m, err := s.checkRiggerBundle(ctx, data)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(data)
	b := &RiggerBundle{
		Name: riggerBundleNamePrefix + hex.EncodeToString(sum[:])[:32], Stack: m.Stack,
		Size: uint64(len(data)), Imported: time.Now().UTC(),
	}

	for _, mod := range m.Modules {
		b.Modules = append(b.Modules, mod.Source+" "+mod.Version)
	}

	err = s.withRiggerBundles(ctx, func(obs jetstream.ObjectStore) error {
		_, err := obs.Put(ctx, jetstream.ObjectMeta{
			Name: b.Name, Description: "bundle of the stack " + m.Stack,
			Metadata: map[string]string{"stack": m.Stack, "modules": strings.Join(b.Modules, ", ")},
		}, bytes.NewReader(data))

		return err
	})
	if err != nil {
		return nil, err
	}

	if r, err := s.riggerRegistriesOf(); err == nil {
		_ = r.unpackBundles(ctx)
	}

	return b, nil
}

// checkRiggerBundle reads a bundle, bounded, and checks that each module of
// it has the digest its manifest says, in a directory of its own.
func (s *Server) checkRiggerBundle(ctx context.Context, data []byte) (*bundle.Manifest, error) {
	m, files, err := bundle.ReadBounded(bytes.NewReader(data), riggerBundleMaxUnpacked)
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	eng, err := s.riggerFetchEngine()
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp("", "norca-rigger-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	for i, mod := range m.Modules {
		dir := fmt.Sprintf("%s/%d", tmp, i)
		if err := bundle.WriteTree(dir, files[mod.Commit]); err != nil {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%s %s: %v", mod.Source, mod.Version, err)
		}

		digest, err := eng.ModuleDigest(ctx, os.DirFS(dir), mod.Path)
		if err != nil {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%s %s: %v", mod.Source, mod.Version, err)
		}

		if digest != mod.Digest {
			return nil, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"%s %s: the bundle holds content of the digest %s, its manifest says %s", mod.Source, mod.Version, digest, mod.Digest)
		}
	}

	return m, nil
}

// listRiggerBundles lists the bundles of the object store.
func (s *Server) listRiggerBundles(ctx context.Context) ([]*RiggerBundle, error) {
	var out []*RiggerBundle

	err := s.withRiggerBundles(ctx, func(obs jetstream.ObjectStore) error {
		infos, err := obs.List(ctx)
		if errors.Is(err, jetstream.ErrNoObjectsFound) {
			return nil
		}

		if err != nil {
			return err
		}

		for _, info := range infos {
			b := &RiggerBundle{Name: info.Name, Size: info.Size, Imported: info.ModTime, Stack: info.Metadata["stack"]}
			if mods := info.Metadata["modules"]; mods != "" {
				b.Modules = strings.Split(mods, ", ")
			}

			out = append(out, b)
		}

		return nil
	})

	return out, err
}

// unpackBundles unpacks into the cache of this server the bundles of the
// object store it has not unpacked yet, by the commit of each module. A
// bundle that can't be read is skipped (it was checked when it was
// imported). Without a bus there is nothing to unpack.
func (r *riggerRegistries) unpackBundles(ctx context.Context) error {
	if r.srv.bus == nil || !r.srv.bus.JetStreamReady() || r.srv.config.Bus.RiggerBundlesBytes() <= 0 {
		return nil
	}

	return r.srv.withRiggerBundles(ctx, func(obs jetstream.ObjectStore) error {
		infos, err := obs.List(ctx)
		if errors.Is(err, jetstream.ErrNoObjectsFound) {
			return nil
		}

		if err != nil {
			return err
		}

		for _, info := range infos {
			key := info.Name + "@" + strconv.FormatUint(info.Size, 10)

			r.mu.Lock()
			done := r.unpacked[key]
			r.mu.Unlock()

			if done {
				continue
			}

			data, err := obs.GetBytes(ctx, info.Name)
			if err != nil {
				continue
			}

			m, files, err := bundle.ReadBounded(bytes.NewReader(data), riggerBundleMaxUnpacked)
			if err != nil {
				continue
			}

			for _, mod := range m.Modules {
				dir := r.fetcher.BundleDir(mod.Commit)
				if _, err := os.Stat(dir); err == nil {
					continue
				}

				if err := bundle.WriteTree(dir, files[mod.Commit]); err != nil {
					return err
				}
			}

			r.mu.Lock()
			r.unpacked[key] = true
			r.mu.Unlock()
		}

		return nil
	})
}
