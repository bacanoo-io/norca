// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package catalog provides the server-side pack registry synchronization,
// catalog indexing, UIFormSchema synthesis, and server-side pack rendering
// facade over the internal pack engine.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/jobspec2"
	"github.com/hashicorp/nomad/jobspec2/hclutil"
	"github.com/hashicorp/nomad/pack/internal/pkg/bundle"
	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/manager"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/release"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
	"github.com/zclconf/go-cty/cty"
)

const DefaultRef = caching.DefaultRef

// IndexedPack describes a cached pack discovered during registry scanning.
type IndexedPack struct {
	Registry         string
	Name             string
	Version          string
	Description      string
	AppURL           string
	Category         string
	Digest           string
	Signed           bool
	Signer           string
	TrustedSigner    string
	RequiresNorca    string
	RequiresFeatures []string
	Regions          []string
	HasSchema        bool
}

// RenderInput holds the parameters for rendering a cached pack into an
// api.ReleaseApplyRequest on the server.
type RenderInput struct {
	PackDir       string
	RegistryName  string
	PackName      string
	Version       string
	ReleaseName   string
	Description   string
	Values        map[string]any
	Secrets       []*api.ReleaseVariableSecret
	TrustedKeys   []string
	Atomic        *bool
	Wait          bool
	Timeout       time.Duration
	TakeOwnership bool
	MaxHistory    int
	Regions       []string
}

func policyForKeys(trustedKeys []string) (packconfig.Policy, func()) {
	var keyFiles []string
	var tempFiles []string
	for _, k := range trustedKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, err := os.Stat(k); err == nil {
			keyFiles = append(keyFiles, k)
		} else {
			f, err := os.CreateTemp("", "norca-trusted-key-*.pub")
			if err == nil {
				_, _ = f.WriteString(k)
				_ = f.Close()
				keyFiles = append(keyFiles, f.Name())
				tempFiles = append(tempFiles, f.Name())
			}
		}
	}
	cleanup := func() {
		for _, tf := range tempFiles {
			_ = os.Remove(tf)
		}
	}
	return packconfig.Policy{KeyFiles: keyFiles}, cleanup
}

// SourceAuth holds the credentials of a private registry, read from the
// Variable its authSecret names: username and password (or token) for git
// https and OCI sources, ssh_private_key and known_hosts for git ssh
// sources.
type SourceAuth struct {
	Username      string
	Password      string
	SSHPrivateKey string
	KnownHosts    string
}

// SourceAuthFromItems reads the credentials from the items of a Variable.
func SourceAuthFromItems(items map[string]string) *SourceAuth {
	a := &SourceAuth{
		Username:      items["username"],
		Password:      items["password"],
		SSHPrivateKey: items["ssh_private_key"],
		KnownHosts:    items["known_hosts"],
	}
	if a.Password == "" {
		a.Password = items["token"]
	}
	return a
}

// ValidateSourceAuth checks that the credentials fit the source: git
// sources must be https:// (password or token) or ssh (ssh_private_key and
// known_hosts), OCI sources need a password or token; other types take no
// credentials.
func ValidateSourceAuth(sourceType, source string, a *SourceAuth) error {
	switch sourceType {
	case "git":
		if caching.IsSSHSource(source) {
			if a.SSHPrivateKey == "" || a.KnownHosts == "" {
				return errors.New("an ssh source needs the items ssh_private_key and known_hosts")
			}
			return nil
		}
		u, err := url.Parse(source)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("credentials need an https:// or ssh source")
		}
		if u.User != nil {
			return errors.New("the source must not carry credentials; they come from the auth secret")
		}
		if a.Password == "" {
			return errors.New("an https source needs the item password or token")
		}
		return nil
	case "oci":
		if a.Password == "" {
			return errors.New("an OCI source needs the item password or token")
		}
		return nil
	default:
		return fmt.Errorf("a %s registry takes no credentials", sourceType)
	}
}

// SyncSource synchronizes a registry source ("dir", "git", "oci", "bundle")
// into cacheDir/<registryName>. auth (git and oci only) is sent to the
// host of the source only. The servers' own credentials (environment,
// docker configuration) are never used.
func SyncSource(cacheDir, registryName, sourceType, source, ref string, trustedKeys []string, auth *SourceAuth, seedBuiltin func(destRefDir, ref string) error) error {
	if ref == "" {
		ref = DefaultRef
	}
	if auth != nil {
		if err := ValidateSourceAuth(sourceType, source, auth); err != nil {
			return err
		}
	}
	switch sourceType {
	case "dir":
		return syncDirRegistry(cacheDir, registryName, source, ref, seedBuiltin)
	case "bundle":
		b, err := bundle.Open(source)
		if err != nil {
			return fmt.Errorf("failed to open bundle: %w", err)
		}
		defer b.Close()
		policy, cleanup := policyForKeys(trustedKeys)
		defer cleanup()
		_, err = b.ImportToCache(context.Background(), cacheDir, registryName, policy)
		if err != nil {
			return fmt.Errorf("failed to import bundle: %w", err)
		}
		return nil
	case "git":
		cache, err := caching.NewCache(&caching.CacheConfig{Path: cacheDir})
		if err != nil {
			return err
		}
		opts := &caching.AddOpts{
			RegistryName: registryName,
			Source:       source,
			Ref:          ref,
		}
		if auth != nil {
			opts.GitAuth = &caching.GitAuth{
				Username:      auth.Username,
				Password:      auth.Password,
				SSHPrivateKey: auth.SSHPrivateKey,
				KnownHosts:    auth.KnownHosts,
			}
		}
		if _, err := cache.Add(opts); err != nil {
			return fmt.Errorf("failed to clone git registry: %w", err)
		}
		return nil
	case "oci":
		loc, err := oci.ParseLocation(source)
		if err != nil {
			return fmt.Errorf("invalid OCI source: %w", err)
		}
		opts := &oci.Options{NoDockerConfig: true, NoEnvCredentials: true}
		if auth != nil {
			opts.Username, opts.Password, opts.AuthHost = auth.Username, auth.Password, loc.Host
		}
		repo, err := opts.Repository(loc)
		if err != nil {
			return fmt.Errorf("failed to connect to OCI repository: %w", err)
		}
		tag := loc.Reference
		if tag == "" {
			tag = ref
		}
		packName := filepath.Base(loc.Repository)
		policy, cleanup := policyForKeys(trustedKeys)
		defer cleanup()
		if _, _, err := supplychain.Fetch(context.Background(), repo, packName, tag, supplychain.FetchOptions{
			CacheRoot:      cacheDir,
			Registry:       registryName,
			RegistrySource: source,
			Location:       loc.WithReference(tag).String(),
			Policy:         policy,
		}); err != nil {
			return fmt.Errorf("failed to pull OCI pack: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported registry type %q", sourceType)
	}
}

func syncDirRegistry(cacheDir, registryName, source, ref string, seedBuiltin func(destRefDir, ref string) error) error {
	destRefDir := filepath.Join(cacheDir, registryName, ref)
	if err := os.RemoveAll(destRefDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destRefDir, 0o755); err != nil {
		return err
	}
	if source == "builtin" && seedBuiltin != nil {
		return seedBuiltin(destRefDir, ref)
	}

	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("source directory %q: %w", source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source %q is not a directory", source)
	}

	if _, err := os.Stat(filepath.Join(source, "metadata.hcl")); err == nil {
		loaded, err := loader.Load(source)
		if err != nil {
			return fmt.Errorf("failed to load pack at %q: %w", source, err)
		}
		packName := filepath.Base(source)
		if loaded.Metadata != nil && loaded.Metadata.Pack != nil && loaded.Metadata.Pack.Name != "" {
			packName = loaded.Metadata.Pack.Name
		}
		return copyPackDirectory(source, filepath.Join(destRefDir, packName+"@"+ref))
	}

	scanRoot := source
	if sub, err := os.Stat(filepath.Join(source, "packs")); err == nil && sub.IsDir() {
		scanRoot = filepath.Join(source, "packs")
	}
	entries, err := os.ReadDir(scanRoot)
	if err != nil {
		return err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		packPath := filepath.Join(scanRoot, entry.Name())
		if _, err := os.Stat(filepath.Join(packPath, "metadata.hcl")); err != nil {
			continue
		}
		loaded, err := loader.Load(packPath)
		if err != nil {
			continue
		}
		packName := entry.Name()
		if loaded.Metadata != nil && loaded.Metadata.Pack != nil && loaded.Metadata.Pack.Name != "" {
			packName = loaded.Metadata.Pack.Name
		}
		if err := copyPackDirectory(packPath, filepath.Join(destRefDir, packName+"@"+ref)); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no valid packs with metadata.hcl found in %q", source)
	}
	return nil
}

func copyPackDirectory(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// ScanRegistryPacks scans cacheDir/<registryName> and returns all valid packs.
func ScanRegistryPacks(cacheDir, registryName string, trustedKeys []string) ([]*IndexedPack, error) {
	regDir := filepath.Join(cacheDir, registryName)
	refDirs, err := os.ReadDir(regDir)
	if err != nil {
		return nil, err
	}

	policy, cleanup := policyForKeys(trustedKeys)
	defer cleanup()

	var out []*IndexedPack
	for _, refEntry := range refDirs {
		if !refEntry.IsDir() || strings.HasPrefix(refEntry.Name(), ".") {
			continue
		}
		refPath := filepath.Join(regDir, refEntry.Name())
		packDirs, err := os.ReadDir(refPath)
		if err != nil {
			continue
		}
		for _, pd := range packDirs {
			if !pd.IsDir() || !strings.Contains(pd.Name(), "@") {
				continue
			}
			fullPackDir := filepath.Join(refPath, pd.Name())
			loaded, err := loader.Load(fullPackDir)
			if err != nil || loaded.Metadata == nil || loaded.Metadata.Pack == nil {
				continue
			}
			md := loaded.Metadata.Pack
			ver := md.Version
			if ver == "" {
				_, ver, _ = strings.Cut(pd.Name(), "@")
			}
			entry := &IndexedPack{
				Registry:    registryName,
				Name:        md.Name,
				Version:     ver,
				Description: md.Description,
				Regions:     loaded.Metadata.Regions,
				HasSchema:   loaded.Metadata.VariablesSchema != "" || len(loaded.RootVariableFiles()) > 0,
				Category:    inferPackCategory(md.Name, md.Description),
			}
			if loaded.Metadata.App != nil {
				entry.AppURL = loaded.Metadata.App.URL
			}
			if md.Requires != nil {
				norcaConstraint := md.Requires.Norca
				if norcaConstraint == "" {
					norcaConstraint = md.Requires.NomadFork
				}
				entry.RequiresNorca = norcaConstraint
				entry.RequiresFeatures = md.Requires.Features
			}
			if digest, err := packlock.TreeDigest(fullPackDir, packlock.ContentFilter); err == nil {
				entry.Digest = digest
			}
			if prov, err := supplychain.VerifyCached(fullPackDir, policy); err == nil && prov != nil {
				if prov.Digest != "" {
					entry.Digest = prov.Digest
				}
				if prov.Signature != nil {
					entry.Signed = prov.Signature.Verified
					entry.Signer = prov.SignerKeyID()
					entry.TrustedSigner = prov.Signature.KeySource
				}
			}
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}

// SynthesizeFormSchema loads a pack directory and returns its unified UIFormSchema.
func SynthesizeFormSchema(packDir string) (map[string]any, error) {
	pm := manager.NewPackManager(&manager.Config{
		Path:           packDir,
		AllowUnsetVars: true,
	}, nil)
	return pm.UIFormSchema()
}

// RenderPack renders a cached pack into an api.ReleaseApplyRequest.
func RenderPack(in RenderInput) (*api.ReleaseApplyRequest, error) {
	policy, cleanup := policyForKeys(in.TrustedKeys)
	defer cleanup()

	provenance, err := supplychain.VerifyCached(in.PackDir, policy)
	if err != nil {
		return nil, fmt.Errorf("pack verification failed: %w", err)
	}

	cliVars := make(map[string]string, len(in.Values))
	for k, v := range in.Values {
		if v == nil {
			continue
		}
		switch tv := v.(type) {
		case string:
			cliVars[k] = tv
		default:
			b, err := json.Marshal(tv)
			if err != nil {
				return nil, fmt.Errorf("variable %q: %w", k, err)
			}
			cliVars[k] = string(b)
		}
	}

	pm := manager.NewPackManager(&manager.Config{
		Path:            in.PackDir,
		VariableCLIArgs: cliVars,
	}, nil)
	rendered, wErrs := pm.ProcessTemplates(false, true, false)
	if len(wErrs) > 0 {
		msgs := make([]string, 0, len(wErrs))
		for _, we := range wErrs {
			if we != nil && we.Err != nil {
				msgs = append(msgs, we.Err.Error())
			}
		}
		return nil, fmt.Errorf("pack render failed: %s", strings.Join(msgs, "; "))
	}

	allTpl := make(map[string]string, rendered.LenParentRenders()+rendered.LenDependentRenders())
	for k, v := range rendered.DependentRenders() {
		allTpl[k] = v
	}
	for k, v := range rendered.ParentRenders() {
		allTpl[k] = v
	}

	jobTpls := map[string]string{}
	var otherNames []string
	for name, content := range allTpl {
		if release.IsResourceTemplate(name) {
			if strings.TrimSpace(content) != "" {
				otherNames = append(otherNames, name)
			}
		} else if strings.TrimSpace(content) != "" {
			jobTpls[name] = content
		}
	}
	sort.Strings(otherNames)

	digest := ""
	signer := ""
	if provenance != nil {
		digest = provenance.Digest
		signer = provenance.SignerKeyID()
	} else if d, dErr := packlock.TreeDigest(in.PackDir, packlock.ContentFilter); dErr == nil {
		digest = d
	}

	relPack := &api.ReleasePack{
		Name:     in.PackName,
		Version:  in.Version,
		Registry: in.RegistryName,
		Path:     in.PackDir,
		Digest:   digest,
		Signer:   signer,
	}
	if md := pm.Metadata(); md != nil && md.Pack != nil {
		if md.Pack.Version != "" {
			relPack.Version = md.Pack.Version
		}
		if rq := md.Pack.Requires; rq != nil {
			relPack.Requires = &api.ReleasePackRequires{Norca: rq.Norca, Features: rq.Features}
			if relPack.Requires.Norca == "" {
				relPack.Requires.Norca = rq.NomadFork
			}
		}
	}
	if origin, err := supplychain.ReadOrigin(in.PackDir); err == nil && origin != nil {
		for _, sig := range origin.Signatures {
			relPack.Signatures = append(relPack.Signatures, &api.ReleasePackSignature{
				Payload:   string(sig.Payload),
				Signature: sig.Signature,
			})
		}
	}

	// Variables with a secret block go to their Variable item.
	secrets := manager.MergeSecrets(in.Secrets, pm.Secrets())
	for _, name := range pm.SecretVariables() {
		delete(cliVars, name)
	}
	valuesMap, valuesHash := release.Values(cliVars, nil)
	for k, rawVal := range in.Values {
		if cur, ok := valuesMap[k]; ok && cur != release.Redacted {
			valuesMap[k] = rawVal
		}
	}
	_, varValues := pm.VariablesSchemas()
	varSchemas := pm.SynthesizedVariablesSchemas()

	outReq := &api.ReleaseApplyRequest{
		Pack:             relPack,
		Description:      in.Description,
		Values:           valuesMap,
		ValuesHash:       valuesHash,
		VariablesSchemas: varSchemas,
		VariableValues:   varValues,
		Secrets:          secrets,
		Atomic:           in.Atomic,
		Wait:             in.Wait,
		Timeout:          in.Timeout,
		TakeOwnership:    in.TakeOwnership,
		MaxHistory:       in.MaxHistory,
		Regions:          in.Regions,
	}

	for _, name := range otherNames {
		res, err := release.ParseResource(name, allTpl[name])
		if err != nil {
			return nil, fmt.Errorf("failed to parse resource template %s: %w", name, err)
		}
		outReq.Resources = append(outReq.Resources, res)
	}

	jobNames := make([]string, 0, len(jobTpls))
	for name := range jobTpls {
		jobNames = append(jobNames, name)
	}
	sort.Strings(jobNames)
	for _, name := range jobNames {
		withMeta := setPackJobHCLMeta(jobTpls[name], packJobMeta{
			PackPath:       in.PackDir,
			PackName:       in.PackName,
			RegistryName:   in.RegistryName,
			DeploymentName: in.ReleaseName,
			PackRef:        relPack.Version,
			PackDigest:     relPack.Digest,
			SignerKeyID:    relPack.Signer,
		})
		ncJob, err := jobspec2.ParseWithConfig(&jobspec2.ParseConfig{
			Path:    name,
			Body:    []byte(withMeta),
			AllowFS: false,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to parse job template %s: %w", name, err)
		}
		hasNS := ncJob.Namespace != nil && *ncJob.Namespace != ""
		hasReg := ncJob.Region != nil && *ncJob.Region != ""
		ncJob.Canonicalize()
		if !hasNS {
			ncJob.Namespace = nil
		}
		if !hasReg {
			ncJob.Region = nil
		}
		outReq.Resources = append(outReq.Resources, &api.ReleaseResource{
			Type:   api.ReleaseResourceJob,
			Source: name,
			Job:    ncJob,
			Submission: &api.JobSubmission{
				Source: withMeta,
				Format: "hcl2",
			},
		})
	}

	if len(outReq.Resources) == 0 {
		return nil, fmt.Errorf("pack rendered no resources")
	}
	release.SortResources(outReq.Resources)
	return outReq, nil
}

type packJobMeta struct {
	PackPath       string
	PackName       string
	RegistryName   string
	DeploymentName string
	PackRef        string
	PackDigest     string
	SignerKeyID    string
}

func setPackJobHCLMeta(jobHCL string, meta packJobMeta) string {
	file, diags := hclsyntax.ParseConfig([]byte(jobHCL), "", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return jobHCL
	}
	content, diags := file.Body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{{Type: "job", LabelNames: []string{""}}},
	})
	if diags.HasErrors() {
		return jobHCL
	}

	result := map[string]cty.Value{}
	deleteBlock := false
	for _, b := range content.Blocks {
		cont, _, diags := b.Body.PartialContent(&hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{{Name: "meta", Required: false}},
			Blocks:     []hcl.BlockHeaderSchema{{Type: "meta"}},
		})
		if diags.HasErrors() || (len(cont.Blocks) == 0 && len(cont.Attributes) == 0) {
			continue
		}
		if len(cont.Attributes) == 0 && len(cont.Blocks) > 0 {
			deleteBlock = true
			ba := hclutil.BlocksAsAttrs(b.Body)
			attrs, diags := ba.JustAttributes()
			if diags.HasErrors() {
				return jobHCL
			}
			var r []map[string]cty.Value
			if diag := gohcl.DecodeExpression(attrs["meta"].Expr, nil, &r); diag.HasErrors() || len(r) == 0 {
				return jobHCL
			}
			result = r[0]
		} else {
			if diag := gohcl.DecodeExpression(cont.Attributes["meta"].Expr, nil, &result); diag.HasErrors() {
				return jobHCL
			}
		}
	}

	wFile, _ := hclwrite.ParseConfig([]byte(jobHCL), "", hcl.Pos{Line: 1, Column: 1})
	if wFile == nil {
		return jobHCL
	}
	var jobBlock *hclwrite.Block
	for _, b := range wFile.Body().Blocks() {
		if b.Type() == "job" {
			jobBlock = b
			break
		}
	}
	if jobBlock == nil || len(jobBlock.Labels()) == 0 {
		return jobHCL
	}
	jobBody := jobBlock.Body()

	result["pack.path"] = cty.StringVal(meta.PackPath)
	result["pack.name"] = cty.StringVal(meta.PackName)
	result["pack.registry"] = cty.StringVal(meta.RegistryName)
	result["pack.deployment_name"] = cty.StringVal(meta.DeploymentName)
	result["pack.job"] = cty.StringVal(jobBlock.Labels()[0])
	result["pack.version"] = cty.StringVal(meta.PackRef)
	if meta.PackDigest != "" {
		result["pack.digest"] = cty.StringVal(meta.PackDigest)
	}
	if meta.SignerKeyID != "" {
		result["pack.signer"] = cty.StringVal(meta.SignerKeyID)
	}
	jobBody.SetAttributeValue("meta", cty.ObjectVal(result))
	if deleteBlock {
		jobBody.RemoveBlock(jobBody.FirstMatchingBlock("meta", nil))
	}
	return string(wFile.Bytes())
}

func inferPackCategory(name, desc string) string {
	lower := strings.ToLower(name + " " + desc)
	switch {
	case strings.Contains(lower, "operator") || strings.Contains(lower, "controller") || strings.Contains(lower, "kind"):
		return "Operators & Controllers"
	case strings.Contains(lower, "postgres") || strings.Contains(lower, "redis") || strings.Contains(lower, "database") || strings.Contains(lower, "cache"):
		return "Databases & Caching"
	case strings.Contains(lower, "loadbalancer") || strings.Contains(lower, "ingress") || strings.Contains(lower, "proxy") || strings.Contains(lower, "mesh") || strings.Contains(lower, "consul"):
		return "Networking & Mesh"
	default:
		return "Applications"
	}
}
