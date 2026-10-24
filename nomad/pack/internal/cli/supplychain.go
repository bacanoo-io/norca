// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"context"
	"crypto"
	stderrors "errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/term"

	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
)

const (
	// Lock modes of the --lock flag of render, plan and run.
	lockModeVerify  = "verify"
	lockModeRequire = "require"
	lockModeUpdate  = "update"
	lockModeIgnore  = "ignore"

	// EnvLockMode sets the default of the --lock flag.
	EnvLockMode = "NORCA_PACK_LOCK"

	// EnvKeyPassword holds the password of the signing key. COSIGN_PASSWORD
	// is accepted as a fallback for compatibility with cosign.
	EnvKeyPassword    = "NORCA_PACK_KEY_PASSWORD"
	envCosignPassword = "COSIGN_PASSWORD"
)

// addLockFlag adds the --lock flag to the operation options of a command.
func (c *baseCommand) addLockFlag(f *flag.Set) {
	f.EnumSingleVar(&flag.EnumSingleVar{
		Name:    "lock",
		Target:  &c.lockMode,
		Values:  []string{lockModeVerify, lockModeRequire, lockModeUpdate, lockModeIgnore},
		Default: lockModeVerify,
		EnvVar:  EnvLockMode,
		Usage: `How to handle the pack.lock file of the pack. "verify" checks the
				digests of the pack and its vendored dependencies if the pack
				has a lock file, "require" also fails if it has none, "update"
				(re)writes the lock file of a pack in a local directory, and
				"ignore" skips the check.`,
	})
}

// preparePack makes sure the pack described by cfg is available and
// trustworthy before it is used:
//
//   - A pack of a configured OCI registry that is not in the cache yet is
//     downloaded, verified with the registry's trust policy and cached. If no
//     version was requested, the newest version is used.
//   - A cached pack that was downloaded from an OCI registry or imported from
//     a bundle is verified again, offline, against its signed manifest and
//     the current trust policy.
//   - The pack's lock file is handled according to the --lock mode.
//
// It returns the provenance of OCI packs, or nil for other packs.
func (c *baseCommand) preparePack(cfg *caching.PackConfig) (*supplychain.Result, error) {
	var result *supplychain.Result
	exists := func() bool {
		_, err := os.Stat(cfg.Path)
		return err == nil
	}
	if cfg.Registry != caching.DevRegistryName {
		conf, err := packconfig.LoadDefault()
		if err != nil {
			return nil, err
		}
		if reg := conf.Registry(cfg.Registry); reg != nil {
			if err := c.fetchFromRegistry(conf, reg, cfg, false); err != nil {
				return nil, err
			}
		}
		if !exists() {
			// Reported by the command's check that the pack exists.
			return nil, nil
		}
		result, err = supplychain.VerifyCached(cfg.Path, conf.TrustPolicy(cfg.Registry))
		if err != nil {
			return nil, err
		}
		if result == nil && conf.Registry(cfg.Registry) != nil {
			return nil, fmt.Errorf("cached pack %s has no origin record; delete it with \"norca pack registry delete %s --target=%s\" and run the command again",
				cfg.Path, cfg.Registry, cfg.Name)
		}
		if result != nil {
			if id := result.SignerKeyID(); id != "" {
				c.ui.Info(fmt.Sprintf("Pack %s verified: signed by key %s", result.Digest, id))
			} else {
				c.ui.Info(fmt.Sprintf("Pack %s is not signed by a trusted key; the trust policy of registry %q does not require it", result.Digest, cfg.Registry))
			}
		}
	}

	if !exists() {
		return nil, nil
	}
	if err := c.handleLock(cfg); err != nil {
		return nil, err
	}
	return result, nil
}

// provenanceDigest returns the manifest digest of an OCI pack, or "".
func provenanceDigest(r *supplychain.Result) string {
	if r == nil {
		return ""
	}
	return r.Digest
}

// fetchFromRegistry resolves the version of a pack of an OCI registry and
// downloads it into the cache if it is not cached yet.
func (c *baseCommand) fetchFromRegistry(conf *packconfig.Config, reg *packconfig.Registry, cfg *caching.PackConfig, force bool) error {
	ctx := c.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cacheRoot := caching.DefaultCachePath()
	loc := reg.Location().Child(cfg.Name)
	opts := conf.Options(reg)

	repo, err := opts.Repository(loc)
	if err != nil {
		return err
	}

	tag := cfg.Ref
	if tag == "" || tag == caching.DefaultRef {
		tag, err = supplychain.ResolveTag(ctx, repo, "")
		if err != nil {
			// Offline: fall back to the newest cached version.
			cached := newestCachedVersion(cacheRoot, reg.Name, cfg.Name)
			if cached == "" {
				return fmt.Errorf("failed to resolve the newest version of %s: %w", loc, err)
			}
			c.ui.Warning(fmt.Sprintf("Could not reach %s (%v); using the cached version %s", loc, err, cached))
			tag = cached
		}
	}
	cfg.Ref = tag
	cfg.Path = supplychain.PackDir(cacheRoot, reg.Name, cfg.Name, tag)

	if _, err := os.Stat(cfg.Path); err == nil && !force {
		return nil
	}
	c.ui.Info(fmt.Sprintf("Downloading %s", loc.WithReference(tag)))
	_, res, err := supplychain.Fetch(ctx, repo, cfg.Name, tag, supplychain.FetchOptions{
		CacheRoot:      cacheRoot,
		Registry:       reg.Name,
		RegistrySource: reg.URL,
		Location:       loc.String(),
		Repository:     loc.RepositoryName(),
		Policy:         conf.TrustPolicy(reg.Name),
	})
	if err != nil {
		return err
	}
	c.ui.Info(fmt.Sprintf("Downloaded %s (%s)", loc.WithReference(tag), res.Digest))
	return nil
}

// newestCachedVersion returns the newest version of a pack in the cache.
func newestCachedVersion(cacheRoot, registryName, packName string) string {
	entries, err := os.ReadDir(path.Join(cacheRoot, registryName))
	if err != nil {
		return ""
	}
	var tags []string
	for _, e := range entries {
		ref := caching.UnescapeRef(e.Name())
		if _, err := os.Stat(supplychain.PackDir(cacheRoot, registryName, packName, ref)); err == nil {
			tags = append(tags, ref)
		}
	}
	tag, err := supplychain.NewestTag(tags)
	if err != nil {
		return ""
	}
	return tag
}

// handleLock applies the --lock mode to the pack.
func (c *baseCommand) handleLock(cfg *caching.PackConfig) error {
	mode := c.lockMode
	if mode == "" {
		mode = lockModeVerify
	}
	if mode == lockModeIgnore {
		return nil
	}
	p, err := loader.Load(cfg.Path)
	if err != nil {
		return fmt.Errorf("failed to load pack: %w", err)
	}

	switch mode {
	case lockModeUpdate:
		if cfg.Registry != caching.DevRegistryName {
			return fmt.Errorf("--lock=%s is only supported for packs in a local directory", lockModeUpdate)
		}
		prev, err := packlock.Read(cfg.Path)
		if err != nil && !stderrors.Is(err, os.ErrNotExist) {
			return err
		}
		lock, err := packlock.Generate(cfg.Path, p.Metadata, prev)
		if err != nil {
			return err
		}
		if err := lock.Write(cfg.Path); err != nil {
			return err
		}
		c.ui.Info(fmt.Sprintf("Wrote %s", packlock.Path(cfg.Path)))
		return nil
	default:
		_, err := packlock.Verify(cfg.Path, p.Metadata)
		if stderrors.Is(err, os.ErrNotExist) {
			if mode == lockModeRequire {
				return fmt.Errorf("pack has no %s, but --lock=%s is set", packlock.FileName, lockModeRequire)
			}
			return nil
		}
		return err
	}
}

// registryFlags are the options of commands that talk to OCI registries
// directly (push, sign, verify, bundle).
type registryFlags struct {
	registry      string
	plainHTTP     bool
	caCert        string
	tlsSkipVerify bool
	username      string
	password      string
}

func (r *registryFlags) addFlags(f *flag.Set) {
	f.StringVar(&flag.StringVar{
		Name:   "registry",
		Target: &r.registry,
		Usage: `Name of a configured OCI registry (see "norca pack registry add").
				Pack references without an oci:// prefix are looked up in it.`,
	})
	f.BoolVar(&flag.BoolVar{
		Name:   "plain-http",
		Target: &r.plainHTTP,
		Usage:  `Connect to the OCI registry over plain HTTP instead of HTTPS.`,
	})
	f.StringVar(&flag.StringVar{
		Name:   "registry-ca-cert",
		Target: &r.caCert,
		Usage:  `Path to a PEM encoded CA certificate to verify the OCI registry with.`,
	})
	f.BoolVar(&flag.BoolVar{
		Name:   "registry-tls-skip-verify",
		Target: &r.tlsSkipVerify,
		Usage:  `Do not verify the TLS certificate of the OCI registry. Not recommended.`,
	})
	f.StringVar(&flag.StringVar{
		Name:   "registry-username",
		Target: &r.username,
		Usage: fmt.Sprintf(`User name for the OCI registry. Overrides %s and the
				docker configuration.`, oci.EnvRegistryUsername),
	})
	f.StringVar(&flag.StringVar{
		Name:   "registry-password",
		Target: &r.password,
		Usage: fmt.Sprintf(`Password or token for the OCI registry. Prefer the %s
				environment variable, which does not show up in the process list.`, oci.EnvRegistryPassword),
	})
}

// apply overrides the endpoint's connection options with the flags.
func (r *registryFlags) apply(o *oci.Options) {
	if r.plainHTTP {
		o.PlainHTTP = true
	}
	if r.caCert != "" {
		o.CACertFile = r.caCert
	}
	if r.tlsSkipVerify {
		o.TLSSkipVerify = true
	}
	if r.username != "" || r.password != "" {
		o.Username, o.Password = r.username, r.password
	}
}

// ociTarget is a resolved reference to a pack in an OCI registry.
type ociTarget struct {
	endpoint *supplychain.Endpoint
	// name is the last path element of the repository, i.e. the pack name.
	name string
}

// resolveTarget resolves a pack reference: either "oci://host/repo/name[:tag]"
// or "name[:tag]" in the registry named with --registry. tag overrides the
// tag of the reference if set.
func (r *registryFlags) resolveTarget(arg, tag string) (*ociTarget, error) {
	conf, err := packconfig.LoadDefault()
	if err != nil {
		return nil, err
	}
	var loc oci.Location
	if oci.IsOCI(arg) {
		if loc, err = oci.ParseLocation(arg); err != nil {
			return nil, err
		}
		if loc.Repository == "" {
			return nil, fmt.Errorf("%q names no repository", arg)
		}
	} else {
		if r.registry == "" {
			return nil, fmt.Errorf("%q is not an oci:// reference; use --registry to name a configured OCI registry", arg)
		}
		reg := conf.Registry(r.registry)
		if reg == nil {
			return nil, fmt.Errorf("OCI registry %q is not configured in %s", r.registry, conf.Path())
		}
		name, ref, _ := strings.Cut(arg, ":")
		if name == "" || strings.ContainsAny(name, "/@") {
			return nil, fmt.Errorf("invalid pack reference %q", arg)
		}
		loc = reg.Location().Child(name).WithReference(ref)
	}
	if tag != "" {
		loc = loc.WithReference(tag)
	}
	ep := supplychain.EndpointFor(conf, loc)
	if r.registry != "" && ep.RegistryName == "" {
		if reg := conf.Registry(r.registry); reg != nil {
			ep.RegistryName = reg.Name
			ep.Options = conf.Options(reg)
			ep.Policy = conf.TrustPolicy(reg.Name)
		}
	}
	r.apply(ep.Options)
	return &ociTarget{endpoint: ep, name: path.Base(loc.Repository)}, nil
}

// keyPassword returns the password of a signing key from the environment,
// or asks for it on an interactive terminal.
func keyPassword(confirm bool) ([]byte, error) {
	if v, ok := os.LookupEnv(EnvKeyPassword); ok {
		return []byte(v), nil
	}
	if v, ok := os.LookupEnv(envCosignPassword); ok {
		return []byte(v), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("no key password: set %s or run interactively", EnvKeyPassword)
	}
	fmt.Fprint(os.Stderr, "Enter password for private key: ")
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	if confirm {
		fmt.Fprint(os.Stderr, "Enter password for private key again: ")
		again, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, err
		}
		if string(again) != string(pw) {
			return nil, stderrors.New("passwords do not match")
		}
	}
	return pw, nil
}

// loadSigningKey loads and decrypts a cosign compatible private key.
func loadSigningKey(file string) (*signingKey, error) {
	pw, err := keyPassword(false)
	if err != nil {
		return nil, err
	}
	key, err := signing.LoadPrivateKey(file, pw)
	if err != nil {
		return nil, fmt.Errorf("failed to load signing key %s: %w", file, err)
	}
	id, err := signing.KeyID(key.Public())
	if err != nil {
		return nil, err
	}
	return &signingKey{Signer: key, id: id}, nil
}

type signingKey struct {
	crypto.Signer
	id string
}
