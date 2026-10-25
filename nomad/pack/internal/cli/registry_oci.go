// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/terminal"
)

// ociRegistryAddFlags are the "registry add" options for OCI registries.
type ociRegistryAddFlags struct {
	plainHTTP        bool
	caCert           string
	tlsSkipVerify    bool
	trustKeys        []string
	requireSignature bool
}

func (o *ociRegistryAddFlags) addFlags(f *flag.Set) {
	f.BoolVar(&flag.BoolVar{
		Name:   "plain-http",
		Target: &o.plainHTTP,
		Usage:  `OCI registries only: connect over plain HTTP instead of HTTPS.`,
	})
	f.StringVar(&flag.StringVar{
		Name:   "registry-ca-cert",
		Target: &o.caCert,
		Usage:  `OCI registries only: PEM encoded CA certificate to verify the registry with.`,
	})
	f.BoolVar(&flag.BoolVar{
		Name:   "registry-tls-skip-verify",
		Target: &o.tlsSkipVerify,
		Usage:  `OCI registries only: do not verify the registry's TLS certificate.`,
	})
	f.StringSliceVar(&flag.StringSliceVar{
		Name:   "trust-key",
		Target: &o.trustKeys,
		Usage: `OCI registries only: public key file trusted to sign the packs of
				this registry. Can be repeated.`,
	})
	f.BoolVar(&flag.BoolVar{
		Name:   "require-signature",
		Target: &o.requireSignature,
		Usage: `OCI registries only: reject packs of this registry that are not
				signed by a trusted key.`,
	})
}

// addOCI adds an OCI registry to the configuration file and downloads the
// targeted pack, or all packs the registry lists, into the cache.
func (c *RegistryAddCommand) addOCI() int {
	reg := &packconfig.Registry{
		Name:          c.name,
		URL:           strings.TrimSuffix(c.source, "/"),
		PlainHTTP:     c.oci.plainHTTP,
		TLSSkipVerify: c.oci.tlsSkipVerify,
	}
	if c.oci.caCert != "" {
		abs, err := filepath.Abs(c.oci.caCert)
		if err != nil {
			c.ui.ErrorWithContext(err, "invalid CA certificate path")
			return 1
		}
		reg.CACert = abs
	}
	if len(c.oci.trustKeys) > 0 || c.oci.requireSignature {
		reg.Trust = &packconfig.Trust{}
		if c.oci.requireSignature {
			req := true
			reg.Trust.RequireSignature = &req
		}
		for _, k := range c.oci.trustKeys {
			abs, err := filepath.Abs(k)
			if err != nil {
				c.ui.ErrorWithContext(err, "invalid key path")
				return 1
			}
			if _, err := os.Stat(abs); err != nil {
				c.ui.ErrorWithContext(err, "invalid trusted key")
				return 1
			}
			reg.Trust.PublicKeys = append(reg.Trust.PublicKeys, abs)
		}
	}
	if loc, err := oci.ParseLocation(reg.URL); err != nil || loc.Reference != "" {
		if err == nil {
			err = fmt.Errorf("the registry URL must not contain a tag or digest")
		}
		c.ui.ErrorWithContext(err, "invalid OCI registry URL")
		return 1
	}

	configPath := packconfig.DefaultPath()
	if err := packconfig.SaveRegistry(configPath, reg); err != nil {
		c.ui.ErrorWithContext(err, "failed to save registry")
		return 1
	}
	c.ui.Info(fmt.Sprintf("OCI registry %q (%s) added to %s.", reg.Name, reg.URL, configPath))

	conf, err := packconfig.LoadDefault()
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to load configuration")
		return 1
	}
	reg = conf.Registry(c.name)

	var names []string
	if c.target != "" {
		names = []string{c.target}
	} else {
		names, err = c.listOCIPacks(reg)
		if err != nil {
			c.ui.Info(fmt.Sprintf("The registry does not list its packs (%v); packs are downloaded when they are first used, e.g. \"norca pack run <pack> --registry=%s\".", err, reg.Name))
			return 0
		}
	}

	table := registryPackTable()
	for _, name := range names {
		cfg := &caching.PackConfig{Registry: reg.Name, Name: name, Ref: c.ref}
		if err := c.fetchFromRegistry(conf, reg, cfg, true); err != nil {
			c.ui.ErrorWithContext(err, fmt.Sprintf("failed to download pack %s", name))
			return 1
		}
		table.Rows = append(table.Rows, []string{name, cfg.Ref, cfg.Ref, "", reg.Name, reg.URL})
	}
	if len(table.Rows) > 0 {
		c.ui.Table(table)
		c.ui.Info(fmt.Sprintf("Try running one of the packs you just added like this\n\n  norca pack run %s --registry=%s --ref=%s", table.Rows[0][0], reg.Name, table.Rows[0][1]))
	}
	return 0
}

// listOCIPacks returns the names of the repositories directly below the
// registry URL, using the registry's catalog API, which not every registry
// offers.
func (c *baseCommand) listOCIPacks(reg *packconfig.Registry) ([]string, error) {
	conf, err := packconfig.LoadDefault()
	if err != nil {
		return nil, err
	}
	loc := reg.Location()
	client, err := conf.Options(reg).Registry(loc)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if loc.Repository != "" {
		prefix = loc.Repository + "/"
	}
	var names []string
	err = client.Repositories(c.Ctx, "", func(repos []string) error {
		for _, r := range repos {
			if rest, ok := strings.CutPrefix(r, prefix); ok && rest != "" && !strings.Contains(rest, "/") {
				names = append(names, rest)
			}
		}
		return nil
	})
	sort.Strings(names)
	return names, err
}

// cachedPackNames returns the names of the packs of a registry in the cache.
func cachedPackNames(registryName string) []string {
	root := path.Join(caching.DefaultCachePath(), registryName)
	refs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, ref := range refs {
		if !ref.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(path.Join(root, ref.Name()))
		for _, e := range entries {
			escaped, _, ok := strings.Cut(e.Name(), "@")
			if !ok || !e.IsDir() {
				continue
			}
			name, err := caching.UnescapePackName(escaped)
			if err == nil && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// updateOCI downloads the newest version (or --ref) of the targeted pack or of
// every cached pack of an OCI registry.
func (c *RegistryUpdateCommand) updateOCI(conf *packconfig.Config, reg *packconfig.Registry) int {
	names := cachedPackNames(reg.Name)
	if c.target != "" {
		names = []string{c.target}
	}
	if len(names) == 0 {
		var err error
		if names, err = c.listOCIPacks(reg); err != nil || len(names) == 0 {
			c.ui.Info("No packs of this registry are cached; packs are downloaded when they are first used.")
			return 0
		}
	}
	table := registryPackTable()
	for _, name := range names {
		cfg := &caching.PackConfig{Registry: reg.Name, Name: name, Ref: c.ref}
		if err := c.fetchFromRegistry(conf, reg, cfg, true); err != nil {
			c.ui.ErrorWithContext(err, fmt.Sprintf("failed to update pack %s", name))
			return 1
		}
		table.Rows = append(table.Rows, []string{name, cfg.Ref, cfg.Ref, "", reg.Name, reg.URL})
	}
	c.ui.Info("Registry successfully updated in cache.")
	c.ui.Table(table)
	return 0
}

// addConfiguredRegistryRows adds the configured OCI registries that have no
// packs in the cache to a registry table.
func addConfiguredRegistryRows(table *terminal.Table, cached []*caching.Registry) {
	conf, err := packconfig.LoadDefault()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, r := range cached {
		seen[r.Name] = true
	}
	for _, r := range conf.Registries {
		if !seen[r.Name] {
			table.Rows = append(table.Rows, []string{r.Name, "-", "-", r.URL})
		}
	}
}
