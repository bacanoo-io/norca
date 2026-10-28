// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/pack/catalog"
	"github.com/hashicorp/nomad/version"
)

const (
	PackRegistryPhaseReady   = "Ready"
	PackRegistryPhaseSyncing = "Syncing"
	PackRegistryPhaseError   = "Error"

	BuiltinRegistrySource = "builtin"

	// MinPackRegistrySyncInterval is the shortest periodic sync interval.
	MinPackRegistrySyncInterval = time.Minute

	// packRegistrySyncCheck is how often the leader looks for registries
	// due for a periodic sync.
	packRegistrySyncCheck = 30 * time.Second

	// packCacheGenerationFile records in a registry cache the
	// syncGeneration of the status it was fetched for.
	packCacheGenerationFile = ".generation"
)

// PackRegistry is the typed view of a packs.norca.io/Registry custom object.
type PackRegistry struct {
	ID          string              `json:"ID"`
	Name        string              `json:"Name"`
	Spec        *PackRegistrySpec   `json:"Spec"`
	Status      *PackRegistryStatus `json:"Status"`
	CreateTime  int64               `json:"CreateTime"`
	ModifyTime  int64               `json:"ModifyTime"`
	CreateIndex uint64              `json:"CreateIndex"`
	ModifyIndex uint64              `json:"ModifyIndex"`
}

// PackRegistrySpec configures a cluster pack registry source.
type PackRegistrySpec struct {
	Type         string   `json:"type"`                   // "dir" | "git" | "oci" | "bundle"
	Source       string   `json:"source"`                 // "builtin", local path, git URL, oci://..., or bundle path
	DefaultRef   string   `json:"defaultRef,omitempty"`   // default tag/branch ("latest")
	SyncInterval string   `json:"syncInterval,omitempty"` // periodic sync, e.g. "15m"; "" or "0s": on request only
	AuthSecret   string   `json:"authSecret,omitempty"`   // optional Nomad Variable path
	TrustedKeys  []string `json:"trustedKeys,omitempty"`
}

// PackRegistryStatus records the sync state and indexed packs of a registry.
type PackRegistryStatus struct {
	Phase        string `json:"phase"` // "Ready" | "Syncing" | "Error"
	Message      string `json:"message,omitempty"`
	LastSyncedAt string `json:"lastSyncedAt,omitempty"` // the last sync attempt

	// LastGoodSyncAt is the last successful sync: the packs are those it
	// found, also after failed syncs.
	LastGoodSyncAt string `json:"lastGoodSyncAt,omitempty"`

	// SyncGeneration counts the successful syncs. A server whose cache was
	// fetched for another generation fetches the registry again.
	SyncGeneration int64 `json:"syncGeneration,omitempty"`

	PackCount int                 `json:"packCount"`
	Packs     []*PackCatalogEntry `json:"packs,omitempty"`
}

// syncInterval returns the periodic sync interval of the spec; 0 syncs on
// request only.
func (s *PackRegistrySpec) syncInterval() (time.Duration, error) {
	if s == nil || strings.TrimSpace(s.SyncInterval) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s.SyncInterval))
	if err != nil {
		return 0, fmt.Errorf("invalid sync interval %q: %v", s.SyncInterval, err)
	}
	if d < 0 || (d > 0 && d < MinPackRegistrySyncInterval) {
		return 0, fmt.Errorf("the sync interval must be 0 or at least %s", MinPackRegistrySyncInterval)
	}
	return d, nil
}

// PackCatalogEntry summarizes a pack version in a cluster registry.
type PackCatalogEntry struct {
	Registry        string                       `json:"Registry"`
	Name            string                       `json:"Name"`
	Version         string                       `json:"Version"`
	Description     string                       `json:"Description,omitempty"`
	AppURL          string                       `json:"AppURL,omitempty"`
	Category        string                       `json:"Category,omitempty"`
	Digest          string                       `json:"Digest,omitempty"`
	Signed          bool                         `json:"Signed,omitempty"`
	Signer          string                       `json:"Signer,omitempty"`
	TrustedSigner   string                       `json:"TrustedSigner,omitempty"`
	Requires        *structs.ReleasePackRequires `json:"Requires,omitempty"`
	Compatible      bool                         `json:"Compatible"`
	MissingFeatures []string                     `json:"MissingFeatures,omitempty"`
	Regions         []string                     `json:"Regions,omitempty"`
	HasSchema       bool                         `json:"HasSchema"`
}

// PackDetailResponse is returned by GET /v1/pack/:registry/:pack.
type PackDetailResponse struct {
	Pack         *PackCatalogEntry `json:"Pack"`
	Versions     []string          `json:"Versions"`
	Readme       string            `json:"Readme,omitempty"`
	UIFormSchema map[string]any    `json:"UIFormSchema"`
}

// PackDeployRequest is the request body for POST /v1/release/:name/plan-pack
// and POST /v1/release/:name/deploy-pack.
type PackDeployRequest struct {
	Registry      string                           `json:"Registry"`
	Pack          string                           `json:"Pack"`
	Version       string                           `json:"Version,omitempty"`
	Description   string                           `json:"Description,omitempty"`
	Values        map[string]any                   `json:"Values,omitempty"`
	Secrets       []*structs.ReleaseVariableSecret `json:"Secrets,omitempty"`
	Atomic        *bool                            `json:"Atomic,omitempty"`
	Wait          bool                             `json:"Wait,omitempty"`
	Timeout       time.Duration                    `json:"Timeout,omitempty"`
	TakeOwnership bool                             `json:"TakeOwnership,omitempty"`
	MaxHistory    int                              `json:"MaxHistory,omitempty"`
	Regions       []string                         `json:"Regions,omitempty"`
}

func packRegistryFromObject(obj *structs.CustomObject) (*PackRegistry, error) {
	if obj == nil {
		return nil, nil
	}
	reg := &PackRegistry{
		ID:          obj.ID,
		Name:        obj.Name,
		Spec:        &PackRegistrySpec{},
		Status:      &PackRegistryStatus{},
		CreateTime:  obj.CreateTime,
		ModifyTime:  obj.ModifyTime,
		CreateIndex: obj.CreateIndex,
		ModifyIndex: obj.ModifyIndex,
	}
	if len(obj.Spec) > 0 {
		b, err := json.Marshal(obj.Spec)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, reg.Spec); err != nil {
			return nil, err
		}
	}
	if len(obj.Status) > 0 {
		b, err := json.Marshal(obj.Status)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, reg.Status); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func (r *PackRegistry) objectParts() (spec, status map[string]any, err error) {
	if r.Spec != nil {
		b, err := json.Marshal(r.Spec)
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(b, &spec); err != nil {
			return nil, nil, err
		}
	}
	if r.Status != nil {
		b, err := json.Marshal(r.Status)
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(b, &status); err != nil {
			return nil, nil, err
		}
	}
	return spec, status, nil
}

func (m *releaseManager) packCachePath() (string, error) {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.packCacheDir != "" {
		return m.packCacheDir, nil
	}
	if m.srv.config.DataDir != "" {
		dir := filepath.Join(m.srv.config.DataDir, "packs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		m.packCacheDir = dir
		return dir, nil
	}
	dir, err := os.MkdirTemp("", "norca-packs-")
	if err != nil {
		return "", err
	}
	m.packCacheDir = dir
	return dir, nil
}

func (s *Server) authorizePackCatalogRead(args structs.RequestWithIdentity) error {
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj == nil {
		return nil
	}
	if aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, structs.PackRegistryKindRef, acl.ObjectCapabilityRead) ||
		aclObj.AllowObjectOperationInAnyNamespace(structs.ReleaseKindRef, acl.ObjectCapabilityRead) ||
		aclObj.AllowObjectOperationInAnyNamespace(structs.ReleaseKindRef, acl.ObjectCapabilityWrite) {
		return nil
	}
	return structs.ErrPermissionDenied
}

func (s *Server) authorizePackRegistryWrite(args structs.RequestWithIdentity) error {
	aclObj, err := s.ResolveACL(args)
	if err != nil {
		return err
	}
	if aclObj == nil {
		return nil
	}
	if !aclObj.AllowObjectOperation(acl.ObjectClusterNamespace, structs.PackRegistryKindRef, acl.ObjectCapabilityWrite) {
		return structs.ErrPermissionDenied
	}
	return nil
}

// ListPackRegistries returns all packs.norca.io/Registry custom objects.
func (s *Server) ListPackRegistries(args *structs.GenericRequest) ([]*PackRegistry, uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackCatalogRead(args); err != nil {
		return nil, 0, err
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, 0, err
	}
	index, err := snap.Index(state.TableCustomObjects)
	if err != nil {
		return nil, 0, err
	}
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", state.SortDefault)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*PackRegistry, 0)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		reg, err := packRegistryFromObject(obj)
		if err != nil {
			continue
		}
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, index, nil
}

// GetPackRegistry returns a single packs.norca.io/Registry by name.
func (s *Server) GetPackRegistry(args *structs.GenericRequest, name string) (*PackRegistry, uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackCatalogRead(args); err != nil {
		return nil, 0, err
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, 0, err
	}
	index, err := snap.Index(state.TableCustomObjects)
	if err != nil {
		return nil, 0, err
	}
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", name)
	if err != nil || obj == nil {
		return nil, index, err
	}
	reg, err := packRegistryFromObject(obj)
	return reg, index, err
}

// UpsertPackRegistry creates or updates a cluster pack registry and immediately syncs its catalog.
func (s *Server) UpsertPackRegistry(args *structs.GenericRequest, reg *PackRegistry) (*PackRegistry, uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackRegistryWrite(args); err != nil {
		return nil, 0, err
	}
	if reg == nil || reg.Spec == nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "missing registry spec")
	}
	if err := structs.ValidateReleaseName(reg.Name); err != nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid registry name: %v", err)
	}
	reg.Spec.Type = strings.ToLower(strings.TrimSpace(reg.Spec.Type))
	reg.Spec.Source = strings.TrimSpace(reg.Spec.Source)
	switch reg.Spec.Type {
	case "dir", "git", "oci", "bundle":
	default:
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "invalid registry type %q: must be dir, git, oci or bundle", reg.Spec.Type)
	}
	if reg.Spec.Source == "" {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "registry source is required")
	}
	if reg.Spec.DefaultRef == "" {
		reg.Spec.DefaultRef = catalog.DefaultRef
	}
	if _, err := reg.Spec.syncInterval(); err != nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	if err := s.releases.ensureKinds(); err != nil {
		return nil, 0, err
	}

	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, 0, err
	}
	existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", reg.Name)
	if err != nil {
		return nil, 0, err
	}
	var expected uint64
	var prev *PackRegistryStatus
	if existing != nil {
		expected = existing.ModifyIndex
		reg.ID = existing.ID
		reg.CreateTime = existing.CreateTime
		if old, err := packRegistryFromObject(existing); err == nil {
			prev = old.Status
		}
	}

	reg.Status = s.syncPackRegistryCatalog(reg.Name, reg.Spec, prev)

	specMap, statusMap, err := reg.objectParts()
	if err != nil {
		return nil, 0, err
	}
	obj := &structs.CustomObject{
		ID:         reg.ID,
		Group:      structs.ReleaseObjectGroup,
		Kind:       structs.PackRegistryKind,
		Name:       reg.Name,
		ManagedBy:  structs.ReleaseManagedBy,
		Spec:       specMap,
		Status:     statusMap,
		CreateTime: reg.CreateTime,
	}
	if obj.ID == "" {
		obj.ID = uuid.Generate()
	}
	written, err := s.releases.writeObject(obj, expected)
	if err != nil {
		return nil, 0, err
	}
	out, err := packRegistryFromObject(written)
	return out, written.ModifyIndex, err
}

// SyncPackRegistry re-synchronizes an existing pack registry and updates its status.
func (s *Server) SyncPackRegistry(args *structs.GenericRequest, name string) (*PackRegistry, uint64, error) {
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
	existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", name)
	if err != nil {
		return nil, 0, err
	}
	if existing == nil {
		return nil, 0, structs.NewErrRPCCodedf(http.StatusNotFound, "pack registry %q not found", name)
	}
	return s.syncPackRegistryObject(existing)
}

// syncPackRegistryObject syncs a registry and writes its status.
func (s *Server) syncPackRegistryObject(existing *structs.CustomObject) (*PackRegistry, uint64, error) {
	reg, err := packRegistryFromObject(existing)
	if err != nil {
		return nil, 0, err
	}
	reg.Status = s.syncPackRegistryCatalog(reg.Name, reg.Spec, reg.Status)
	specMap, statusMap, err := reg.objectParts()
	if err != nil {
		return nil, 0, err
	}
	obj := *existing
	obj.Spec = specMap
	obj.Status = statusMap
	written, err := s.releases.writeObject(&obj, existing.ModifyIndex)
	if err != nil {
		return nil, 0, err
	}
	out, err := packRegistryFromObject(written)
	return out, written.ModifyIndex, err
}

// DeletePackRegistry removes a pack registry object and its cached files.
func (s *Server) DeletePackRegistry(args *structs.GenericRequest, name string) (uint64, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return 0, structs.ErrPermissionDenied
	}
	if err := s.authorizePackRegistryWrite(args); err != nil {
		return 0, err
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return 0, err
	}
	existing, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", name)
	if err != nil {
		return 0, err
	}
	if existing == nil {
		return 0, structs.NewErrRPCCodedf(http.StatusNotFound, "pack registry %q not found", name)
	}
	if err := s.releases.deleteObject(structs.PackRegistryKind, "", name); err != nil {
		return 0, err
	}
	if cacheDir, err := s.releases.packCachePath(); err == nil {
		_ = os.RemoveAll(filepath.Join(cacheDir, name))
	}
	return max(1, s.raft.AppliedIndex()), nil
}

// ListCatalogPacks returns the aggregated catalog of packs across all (or a single) registry,
// with live compatibility checks against the running server version and features.
func (s *Server) ListCatalogPacks(args *structs.GenericRequest, registryFilter string) ([]*PackCatalogEntry, uint64, error) {
	regs, index, err := s.ListPackRegistries(args)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*PackCatalogEntry, 0)
	for _, r := range regs {
		if registryFilter != "" && r.Name != registryFilter {
			continue
		}
		if r.Status == nil {
			continue
		}
		for _, p := range r.Status.Packs {
			if p == nil {
				continue
			}
			cp := *p
			cp.Registry = r.Name
			cp.Compatible, cp.MissingFeatures = s.evaluatePackCompatibility(cp.Requires)
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registry != out[j].Registry {
			return out[i].Registry < out[j].Registry
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out, index, nil
}

func (s *Server) evaluatePackCompatibility(req *structs.ReleasePackRequires) (bool, []string) {
	if req == nil {
		return true, nil
	}
	missing := make([]string, 0)
	for _, f := range req.Features {
		if f != "" && !slices.Contains(releaseFeatures, f) {
			missing = append(missing, f)
		}
	}
	if err := checkReleaseRequirements(req); err != nil {
		if len(missing) == 0 {
			missing = append(missing, err.Error())
		}
		return false, missing
	}
	return len(missing) == 0, missing
}

// GetCatalogPack loads a specific pack from the registry cache, extracts its README,
// available versions, and synthesizes its unified UIFormSchema.
func (s *Server) GetCatalogPack(args *structs.GenericRequest, registryName, packName, versionRef string) (*PackDetailResponse, uint64, error) {
	reg, index, err := s.GetPackRegistry(args, registryName)
	if err != nil {
		return nil, 0, err
	}
	if reg == nil {
		return nil, index, structs.NewErrRPCCodedf(http.StatusNotFound, "pack registry %q not found", registryName)
	}

	packDir, entry, versions, err := s.resolveCatalogPackDir(reg, packName, versionRef)
	if err != nil {
		return nil, index, err
	}

	uiSchema, err := catalog.SynthesizeFormSchema(packDir)
	if err != nil {
		return nil, index, structs.NewErrRPCCodedf(http.StatusBadRequest, "failed to synthesize pack form schema: %v", err)
	}

	var readme string
	for _, candidate := range []string{"README.md", "readme.md", "README.txt", "outputs.tpl"} {
		if b, err := os.ReadFile(filepath.Join(packDir, candidate)); err == nil && len(b) > 0 {
			readme = string(b)
			break
		}
	}

	cp := *entry
	cp.Compatible, cp.MissingFeatures = s.evaluatePackCompatibility(cp.Requires)

	return &PackDetailResponse{
		Pack:         &cp,
		Versions:     versions,
		Readme:       readme,
		UIFormSchema: uiSchema,
	}, index, nil
}

func (s *Server) resolveCatalogPackDir(reg *PackRegistry, packName, versionRef string) (string, *PackCatalogEntry, []string, error) {
	cacheDir, err := s.releases.packCachePath()
	if err != nil {
		return "", nil, nil, err
	}
	regRoot := filepath.Join(cacheDir, reg.Name)
	if reg, err = s.ensurePackCache(reg, cacheDir); err != nil {
		return "", nil, nil, err
	}

	var versions []string
	var matched *PackCatalogEntry
	if reg.Status != nil {
		for _, p := range reg.Status.Packs {
			if p != nil && p.Name == packName {
				versions = append(versions, p.Version)
				if versionRef == "" || p.Version == versionRef {
					matched = p
				}
			}
		}
	}
	if matched == nil && reg.Status != nil {
		for _, p := range reg.Status.Packs {
			if p != nil && p.Name == packName {
				matched = p
				break
			}
		}
	}

	ref := reg.Spec.DefaultRef
	if ref == "" {
		ref = catalog.DefaultRef
	}
	if versionRef != "" {
		for _, candidateRef := range []string{versionRef, ref, catalog.DefaultRef} {
			candidateDir := filepath.Join(cacheDir, reg.Name, candidateRef, packName+"@"+candidateRef)
			if info, err := os.Stat(candidateDir); err == nil && info.IsDir() {
				if matched == nil {
					matched = &PackCatalogEntry{Registry: reg.Name, Name: packName, Version: versionRef}
				}
				if len(versions) == 0 {
					versions = []string{matched.Version}
				}
				return candidateDir, matched, versions, nil
			}
		}
	}

	refEntries, err := os.ReadDir(regRoot)
	if err == nil {
		for _, re := range refEntries {
			if !re.IsDir() {
				continue
			}
			subDir := filepath.Join(regRoot, re.Name())
			packEntries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			for _, pe := range packEntries {
				if !pe.IsDir() {
					continue
				}
				namePart, verPart, ok := strings.Cut(pe.Name(), "@")
				if !ok || namePart != packName {
					continue
				}
				if versionRef == "" || verPart == versionRef || (matched != nil && matched.Version == versionRef) {
					fullPath := filepath.Join(subDir, pe.Name())
					if matched == nil {
						matched = &PackCatalogEntry{Registry: reg.Name, Name: packName, Version: verPart}
					}
					if len(versions) == 0 {
						versions = []string{matched.Version}
					}
					return fullPath, matched, versions, nil
				}
			}
		}
	}

	return "", nil, nil, structs.NewErrRPCCodedf(http.StatusNotFound, "pack %q (version %q) not found in registry %q", packName, versionRef, reg.Name)
}

// RenderCatalogPack renders a pack from a cluster registry into an api.ReleaseApplyRequest
// ready to be converted and applied by the server.
func (s *Server) RenderCatalogPack(args *structs.GenericRequest, releaseName string, req *PackDeployRequest) (*api.ReleaseApplyRequest, error) {
	if err := s.Authenticate(nil, args); err != nil {
		return nil, structs.ErrPermissionDenied
	}
	if req == nil || req.Registry == "" || req.Pack == "" {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "registry and pack are required")
	}
	if err := structs.ValidateReleaseName(releaseName); err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}

	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, err
	}
	regObj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", req.Registry)
	if err != nil {
		return nil, err
	}
	if regObj == nil {
		return nil, structs.NewErrRPCCodedf(http.StatusNotFound, "pack registry %q not found", req.Registry)
	}
	reg, err := packRegistryFromObject(regObj)
	if err != nil {
		return nil, err
	}

	packDir, entry, _, err := s.resolveCatalogPackDir(reg, req.Pack, req.Version)
	if err != nil {
		return nil, err
	}

	var apiSecrets []*api.ReleaseVariableSecret
	for _, sec := range req.Secrets {
		if sec != nil {
			apiSecrets = append(apiSecrets, &api.ReleaseVariableSecret{
				Namespace: sec.Namespace,
				Path:      sec.Path,
				Items:     sec.Items,
			})
		}
	}

	outReq, err := catalog.RenderPack(catalog.RenderInput{
		PackDir:       packDir,
		RegistryName:  req.Registry,
		PackName:      req.Pack,
		Version:       entry.Version,
		ReleaseName:   releaseName,
		Description:   req.Description,
		Values:        req.Values,
		Secrets:       apiSecrets,
		TrustedKeys:   reg.Spec.TrustedKeys,
		Atomic:        req.Atomic,
		Wait:          req.Wait,
		Timeout:       req.Timeout,
		TakeOwnership: req.TakeOwnership,
		MaxHistory:    req.MaxHistory,
		Regions:       req.Regions,
	})
	if err != nil {
		return nil, structs.NewErrRPCCodedf(http.StatusBadRequest, "%v", err)
	}
	return outReq, nil
}

// syncPackRegistryCatalog fetches a registry into the pack cache and
// indexes its packs. The fetch goes to a staging directory that replaces
// the cache only when it succeeded: after a failed sync the status keeps
// the packs (and generation) of prev and the cache keeps serving them.
func (s *Server) syncPackRegistryCatalog(registryName string, spec *PackRegistrySpec, prev *PackRegistryStatus) *PackRegistryStatus {
	now := time.Now().UTC().Format(time.RFC3339)
	failed := func(msg string) *PackRegistryStatus {
		out := &PackRegistryStatus{Phase: PackRegistryPhaseError, Message: msg, LastSyncedAt: now}
		if prev != nil {
			out.LastGoodSyncAt = prev.LastGoodSyncAt
			out.SyncGeneration = prev.SyncGeneration
			out.PackCount = prev.PackCount
			out.Packs = prev.Packs
		}
		return out
	}
	cacheDir, err := s.releases.packCachePath()
	if err != nil {
		return failed(fmt.Sprintf("failed to initialize pack cache: %v", err))
	}
	s.releases.syncLock.Lock()
	defer s.releases.syncLock.Unlock()

	staging, err := os.MkdirTemp(cacheDir, ".staging-")
	if err != nil {
		return failed(fmt.Sprintf("failed to prepare the pack cache: %v", err))
	}
	defer os.RemoveAll(staging)

	ref := spec.DefaultRef
	if ref == "" {
		ref = catalog.DefaultRef
	}
	if err := catalog.SyncSource(staging, registryName, spec.Type, spec.Source, ref, spec.TrustedKeys, seedBuiltinPacks); err != nil {
		return failed(err.Error())
	}
	indexed, err := catalog.ScanRegistryPacks(staging, registryName, spec.TrustedKeys)
	if err != nil {
		return failed(err.Error())
	}
	generation := int64(1)
	if prev != nil {
		generation = prev.SyncGeneration + 1
	}
	if err := swapPackCache(cacheDir, staging, registryName, generation); err != nil {
		return failed(fmt.Sprintf("failed to update the pack cache: %v", err))
	}

	entries := make([]*PackCatalogEntry, 0, len(indexed))
	for _, item := range indexed {
		entry := &PackCatalogEntry{
			Registry:      item.Registry,
			Name:          item.Name,
			Version:       item.Version,
			Description:   item.Description,
			AppURL:        item.AppURL,
			Category:      item.Category,
			Digest:        item.Digest,
			Signed:        item.Signed,
			Signer:        item.Signer,
			TrustedSigner: item.TrustedSigner,
			Regions:       item.Regions,
			HasSchema:     item.HasSchema,
		}
		if item.RequiresNorca != "" || len(item.RequiresFeatures) > 0 {
			entry.Requires = &structs.ReleasePackRequires{
				Norca:    item.RequiresNorca,
				Features: item.RequiresFeatures,
			}
		}
		entry.Compatible, entry.MissingFeatures = s.evaluatePackCompatibility(entry.Requires)
		entries = append(entries, entry)
	}

	return &PackRegistryStatus{
		Phase:          PackRegistryPhaseReady,
		Message:        fmt.Sprintf("Synchronized %d pack(s) with Norca %s", len(entries), version.GetVersion().VersionNumber()),
		LastSyncedAt:   now,
		LastGoodSyncAt: now,
		SyncGeneration: generation,
		PackCount:      len(entries),
		Packs:          entries,
	}
}

// swapPackCache replaces the cache of a registry with the staged fetch and
// records its generation.
func swapPackCache(cacheDir, staging, registryName string, generation int64) error {
	staged := filepath.Join(staging, registryName)
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staged, packCacheGenerationFile), []byte(strconv.FormatInt(generation, 10)), 0o644); err != nil {
		return err
	}
	target := filepath.Join(cacheDir, registryName)
	old := target + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			return err
		}
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(old, target)
		return err
	}
	return os.RemoveAll(old)
}

// packCacheGeneration returns the generation the cache of a registry was
// fetched for, or 0.
func packCacheGeneration(cacheDir, registryName string) int64 {
	b, err := os.ReadFile(filepath.Join(cacheDir, registryName, packCacheGenerationFile))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// ensurePackCache makes the cache of a registry match its status: a server
// that became leader, or whose cache was fetched for another generation,
// syncs the registry again. On the leader the sync writes the status and
// the returned registry is the synced one.
func (s *Server) ensurePackCache(reg *PackRegistry, cacheDir string) (*PackRegistry, error) {
	if reg.Status != nil && reg.Status.SyncGeneration > 0 &&
		packCacheGeneration(cacheDir, reg.Name) == reg.Status.SyncGeneration {
		return reg, nil
	}
	if !s.IsLeader() {
		// The RPC endpoint forwards pack content reads to the leader;
		// in-process callers on a follower get a fresh local fetch.
		_ = s.syncPackRegistryCatalog(reg.Name, reg.Spec, reg.Status)
		return reg, nil
	}
	snap, err := s.State().Snapshot()
	if err != nil {
		return nil, err
	}
	obj, err := snap.CustomObjectByName(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", reg.Name)
	if err != nil || obj == nil {
		return reg, err
	}
	synced, _, err := s.syncPackRegistryObject(obj)
	if err != nil {
		return nil, err
	}
	if synced.Status == nil || synced.Status.Phase != PackRegistryPhaseReady {
		msg := ""
		if synced.Status != nil {
			msg = synced.Status.Message
		}
		return nil, structs.NewErrRPCCodedf(http.StatusServiceUnavailable,
			"pack registry %q could not be fetched on this server: %s", reg.Name, msg)
	}
	return synced, nil
}

// packRegistrySyncLoop syncs the registries with a sync interval when they
// are due. It runs on the leader.
func (s *Server) packRegistrySyncLoop(stopCh chan struct{}) {
	timer := time.NewTimer(packRegistrySyncCheck)
	defer timer.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		s.syncDuePackRegistries(time.Now())
		timer.Reset(packRegistrySyncCheck)
	}
}

// syncDuePackRegistries syncs the registries whose sync interval passed
// since their last sync attempt.
func (s *Server) syncDuePackRegistries(now time.Time) {
	snap, err := s.State().Snapshot()
	if err != nil {
		return
	}
	iter, err := snap.CustomObjects(nil, structs.ReleaseObjectGroup, structs.PackRegistryKind, "", state.SortDefault)
	if err != nil {
		return
	}
	var due []*structs.CustomObject
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		obj := raw.(*structs.CustomObject)
		reg, err := packRegistryFromObject(obj)
		if err != nil {
			continue
		}
		interval, err := reg.Spec.syncInterval()
		if err != nil || interval == 0 {
			continue
		}
		if reg.Status != nil && reg.Status.LastSyncedAt != "" {
			if last, err := time.Parse(time.RFC3339, reg.Status.LastSyncedAt); err == nil && now.Sub(last) < interval {
				continue
			}
		}
		due = append(due, obj)
	}
	for _, obj := range due {
		if _, _, err := s.syncPackRegistryObject(obj); err != nil {
			s.logger.Warn("periodic pack registry sync failed", "registry", obj.Name, "error", err)
		}
	}
}
