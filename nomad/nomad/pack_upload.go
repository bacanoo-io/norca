// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/pack/catalog"
)

// Packs uploaded to a cluster registry of the type "uploads" (packs
// installed with the CLI from a local directory, or packs without a
// registry). The archive is stored in Raft as chunks, objects of the kind
// packs.norca.io/PackUpload, so that every leader can rebuild the cache of
// the registry from them, and backups keep them; a sync of the registry
// imports every upload into the cache like the other registry types.

const (
	// PackRegistryTypeUploads is the registry type of uploaded packs, and
	// its source.
	PackRegistryTypeUploads = "uploads"

	// DefaultUploadRegistry is the registry uploads go to by default.
	DefaultUploadRegistry = "uploads"

	// packUploadChunkSize keeps a chunk, base64 encoded, below the size
	// limit of custom objects.
	packUploadChunkSize = 160 * 1024
)

// PackUploadSummary describes an upload in the status of its registry.
type PackUploadSummary struct {
	Digest     string   `json:"digest"`
	FileName   string   `json:"fileName,omitempty"`
	Format     string   `json:"format,omitempty"`
	Size       int      `json:"size"`
	UploadedAt string   `json:"uploadedAt,omitempty"`
	Packs      []string `json:"packs,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// PackUploadRequest uploads a pack archive to a registry of the type
// "uploads", created if missing.
type PackUploadRequest struct {
	Registry string
	FileName string
	Data     []byte
	structs.WriteRequest
}

// PackUploadDeleteRequest removes an upload from its registry.
type PackUploadDeleteRequest struct {
	Registry string
	Digest   string
	structs.WriteRequest
}

// PackUploadResponse is the registry after an upload or its removal.
type PackUploadResponse struct {
	Registry *PackRegistry
	Digest   string
	structs.WriteMeta
}

func uploadDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func uploadChunkName(registry, digest string, chunk int) string {
	return fmt.Sprintf("%s-%s-%d", registry, strings.TrimPrefix(digest, "sha256:")[:24], chunk)
}

// UploadPack stores a pack archive in a registry of the type "uploads" and
// syncs the registry. The archive is checked first: it must import into the
// cache (a bundle verified with the trusted keys of the registry, or a tar
// or tar.gz of pack directories) and contain at least one pack.
func (s *Server) UploadPack(args *structs.GenericRequest, req *PackUploadRequest) (*PackRegistry, string, uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, "", 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackRegistryWrite(args); err != nil {
		return nil, "", 0, err
	}
	name := req.Registry
	if name == "" {
		name = DefaultUploadRegistry
	}
	if err := structs.ValidateReleaseName(name); err != nil {
		return nil, "", 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid registry name: %v", err)
	}
	if len(req.Data) == 0 {
		return nil, "", 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "the upload is empty")
	}
	if len(req.Data) > catalog.MaxUploadSize {
		return nil, "", 0, structs.NewErrRPCCodedf(http.StatusRequestEntityTooLarge, "the upload is larger than %d MiB", catalog.MaxUploadSize>>20)
	}
	if err := s.releases.ensureKinds(); err != nil {
		return nil, "", 0, err
	}

	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, "", 0, err
	}
	existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", name)
	if err != nil {
		return nil, "", 0, err
	}
	var trustedKeys []string
	if existing != nil {
		reg, err := packRegistryFromObject(existing)
		if err != nil {
			return nil, "", 0, err
		}
		if reg.Spec.Type != PackRegistryTypeUploads {
			return nil, "", 0, structs.NewErrRPCCodedf(http.StatusBadRequest,
				"pack registry %q is of the type %q; packs can only be uploaded to a registry of the type %q", name, reg.Spec.Type, PackRegistryTypeUploads)
		}
		trustedKeys = reg.Spec.TrustedKeys
	}

	// Check the archive the way the sync will import it.
	format, packs, err := checkUpload(name, req.Data, trustedKeys)
	if err != nil {
		return nil, "", 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	digest := uploadDigest(req.Data)
	if err := s.storeUpload(name, digest, req.FileName, format, req.Data); err != nil {
		return nil, "", 0, err
	}
	s.logger.Info("pack uploaded", "registry", name, "digest", digest, "packs", strings.Join(packs, ","))

	if existing == nil {
		reg, index, err := s.UpsertPackRegistry(args, &PackRegistry{
			Name: name,
			Spec: &PackRegistrySpec{Type: PackRegistryTypeUploads, Source: PackRegistryTypeUploads},
		})
		return reg, digest, index, err
	}
	reg, index, err := s.syncPackRegistryObject(existing)
	return reg, digest, index, err
}

// checkUpload imports an archive into a temporary cache and returns its
// format and packs.
func checkUpload(registry string, data []byte, trustedKeys []string) (string, []string, error) {
	tmp, err := os.MkdirTemp("", "norca-upload-check-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)
	format, err := catalog.ImportUpload(tmp, registry, data, trustedKeys)
	if err != nil {
		return format, nil, err
	}
	indexed, err := catalog.ScanRegistryPacks(tmp, registry, trustedKeys)
	if err != nil {
		return format, nil, err
	}
	if len(indexed) == 0 {
		return format, nil, fmt.Errorf("the upload contains no pack")
	}
	var packs []string
	for _, p := range indexed {
		packs = append(packs, p.Name+"@"+p.Version)
	}
	sort.Strings(packs)
	return format, packs, nil
}

// storeUpload writes the chunks of an archive, unless they are stored
// already (the same archive uploaded again).
func (s *Server) storeUpload(registry, digest, fileName, format string, data []byte) error {
	snap, err := s.State().Snapshot()
	if err != nil {
		return err
	}
	if obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackUploadKind, "", uploadChunkName(registry, digest, 0)); err != nil {
		return err
	} else if obj != nil {
		return nil
	}
	chunks := (len(data) + packUploadChunkSize - 1) / packUploadChunkSize
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < chunks; i++ {
		end := min((i+1)*packUploadChunkSize, len(data))
		obj := &structs.CustomObject{
			ID:        uuid.Generate(),
			Group:     structs.ReleaseObjectGroup,
			Kind:      structs.PackUploadKind,
			Name:      uploadChunkName(registry, digest, i),
			ManagedBy: structs.ReleaseManagedBy,
			Spec: map[string]any{
				"registry":   registry,
				"digest":     digest,
				"fileName":   fileName,
				"format":     format,
				"size":       len(data),
				"chunk":      i,
				"chunks":     chunks,
				"uploadedAt": now,
				"data":       base64.StdEncoding.EncodeToString(data[i*packUploadChunkSize : end]),
			},
		}
		if _, err := s.releases.writeObject(obj, 0); err != nil {
			return fmt.Errorf("storing chunk %d of the upload: %w", i, err)
		}
	}
	return nil
}

// DeletePackUpload removes an upload from its registry and syncs it.
func (s *Server) DeletePackUpload(args *structs.GenericRequest, registry, digest string) (*PackRegistry, uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackRegistryWrite(args); err != nil {
		return nil, 0, err
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, 0, err
	}
	objs, err := uploadObjects(snap, registry)
	if err != nil {
		return nil, 0, err
	}
	found := false
	for _, obj := range objs {
		if specString(obj.Spec, "digest") != digest {
			continue
		}
		found = true
		if err := s.releases.deleteObject(structs.PackUploadKind, "", obj.Name); err != nil {
			return nil, 0, err
		}
	}
	if !found {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound, "registry %q has no upload %s", registry, digest)
	}
	existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", registry)
	if err != nil || existing == nil {
		return nil, 0, err
	}
	return s.syncPackRegistryObject(existing)
}

// deletePackUploads removes every upload of a registry.
func (s *Server) deletePackUploads(registry string) error {
	snap, err := s.State().Snapshot()
	if err != nil {
		return err
	}
	objs, err := uploadObjects(snap, registry)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := s.releases.deleteObject(structs.PackUploadKind, "", obj.Name); err != nil {
			return err
		}
	}
	return nil
}

func uploadObjects(snap *state.StateSnapshot, registry string) ([]*structs.CustomObject, error) {
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.PackUploadKind, "", state.SortDefault)
	if err != nil {
		return nil, err
	}
	var out []*structs.CustomObject
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		if specString(obj.Spec, "registry") == registry {
			out = append(out, obj)
		}
	}
	return out, nil
}

// importUploads imports the uploads of a registry into cacheDir, the oldest
// first, so that a later upload of a pack version replaces an earlier one.
// An upload that is incomplete or does not import is reported in its
// summary and left out.
func (s *Server) importUploads(cacheDir, registry string, trustedKeys []string) ([]*PackUploadSummary, error) {
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, err
	}
	objs, err := uploadObjects(snap, registry)
	if err != nil {
		return nil, err
	}
	type upload struct {
		summary *PackUploadSummary
		chunks  map[int]string
		total   int
	}
	byDigest := map[string]*upload{}
	for _, obj := range objs {
		digest := specString(obj.Spec, "digest")
		u, ok := byDigest[digest]
		if !ok {
			u = &upload{
				summary: &PackUploadSummary{
					Digest:     digest,
					FileName:   specString(obj.Spec, "fileName"),
					Format:     specString(obj.Spec, "format"),
					Size:       specInt(obj.Spec, "size"),
					UploadedAt: specString(obj.Spec, "uploadedAt"),
				},
				chunks: map[int]string{},
				total:  specInt(obj.Spec, "chunks"),
			}
			byDigest[digest] = u
		}
		u.chunks[specInt(obj.Spec, "chunk")] = specString(obj.Spec, "data")
	}
	uploads := make([]*upload, 0, len(byDigest))
	for _, u := range byDigest {
		uploads = append(uploads, u)
	}
	sort.Slice(uploads, func(i, j int) bool {
		a, b := uploads[i].summary, uploads[j].summary
		if a.UploadedAt != b.UploadedAt {
			return a.UploadedAt < b.UploadedAt
		}
		return a.Digest < b.Digest
	})

	out := make([]*PackUploadSummary, 0, len(uploads))
	for _, u := range uploads {
		out = append(out, u.summary)
		var data []byte
		for i := 0; i < u.total; i++ {
			part, ok := u.chunks[i]
			if !ok {
				u.summary.Error = fmt.Sprintf("chunk %d of %d is missing", i, u.total)
				break
			}
			b, err := base64.StdEncoding.DecodeString(part)
			if err != nil {
				u.summary.Error = fmt.Sprintf("chunk %d is invalid", i)
				break
			}
			data = append(data, b...)
		}
		if u.summary.Error != "" {
			continue
		}
		if uploadDigest(data) != u.summary.Digest {
			u.summary.Error = "the chunks don't match the digest"
			continue
		}
		_, packs, err := checkUpload(registry, data, trustedKeys)
		if err != nil {
			u.summary.Error = err.Error()
			continue
		}
		u.summary.Packs = packs
		if _, err := catalog.ImportUpload(cacheDir, registry, data, trustedKeys); err != nil {
			u.summary.Error = err.Error()
		}
	}
	return out, nil
}

func specString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func specInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case uint64:
		return int(v)
	}
	return 0
}
