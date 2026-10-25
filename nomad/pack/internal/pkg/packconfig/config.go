// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package packconfig reads and writes the Norca Pack client configuration
// file. It defines the OCI registries packs are pulled from and pushed to, and
// the trust policy for pack signatures:
//
//	registry "internal" {
//	  url             = "oci://registry.example.internal/packs"
//	  plain_http      = false
//	  ca_cert         = "/etc/norca/registry-ca.pem"
//	  tls_skip_verify = false
//
//	  trust {
//	    require_signature = true
//	    public_keys       = ["/etc/norca/pack-signing.pub"]
//	  }
//	}
//
//	# Default trust policy for OCI registries without a trust block.
//	trust {
//	  require_signature = false
//	  public_keys       = []
//	}
//
// Relative file names are resolved against the directory of the
// configuration file. "norca pack registry add <name> oci://..." adds and
// "norca pack registry delete" removes registry blocks, keeping the rest of the
// file, including comments, as it is.
package packconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
)

// EnvConfigFile overrides the location of the configuration file.
const EnvConfigFile = "NORCA_PACK_CONFIG"

// Config is the content of the configuration file.
type Config struct {
	Registries []*Registry `hcl:"registry,block"`
	Trust      *Trust      `hcl:"trust,block"`

	// path is the file the configuration was read from.
	path string
}

// Registry is an OCI registry definition.
type Registry struct {
	Name          string `hcl:"name,label"`
	URL           string `hcl:"url"`
	PlainHTTP     bool   `hcl:"plain_http,optional"`
	CACert        string `hcl:"ca_cert,optional"`
	TLSSkipVerify bool   `hcl:"tls_skip_verify,optional"`
	Trust         *Trust `hcl:"trust,block"`
}

// Trust is a signature trust policy.
type Trust struct {
	// RequireSignature rejects packs without a valid signature by one of the
	// public keys.
	RequireSignature *bool `hcl:"require_signature,optional"`
	// PublicKeys are files with trusted cosign compatible public keys.
	PublicKeys []string `hcl:"public_keys,optional"`
}

// DefaultPath returns the location of the configuration file.
func DefaultPath() string {
	if p := os.Getenv(EnvConfigFile); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			home = "."
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "norca", "pack", "config.hcl")
}

// Load reads the configuration file at path. A missing file is an empty
// configuration.
func Load(path string) (*Config, error) {
	cfg := &Config{path: path}
	src, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	file, diags := hclparse.NewParser().ParseHCL(src, path)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse %s: %s", path, diags.Error())
	}
	if diags := gohcl.DecodeBody(file.Body, nil, cfg); diags.HasErrors() {
		return nil, fmt.Errorf("failed to decode %s: %s", path, diags.Error())
	}
	seen := map[string]bool{}
	for _, r := range cfg.Registries {
		if seen[r.Name] {
			return nil, fmt.Errorf("%s: registry %q is defined more than once", path, r.Name)
		}
		seen[r.Name] = true
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return cfg, nil
}

// LoadDefault reads the configuration file at DefaultPath.
func LoadDefault() (*Config, error) {
	return Load(DefaultPath())
}

// Path returns the file the configuration was read from.
func (c *Config) Path() string {
	return c.path
}

// Validate checks a registry definition.
func (r *Registry) Validate() error {
	if r.Name == "" {
		return errors.New("registry name is required")
	}
	if _, err := oci.ParseLocation(r.URL); err != nil {
		return fmt.Errorf("registry %q: %w", r.Name, err)
	}
	return nil
}

// Registry returns the named registry definition, or nil.
func (c *Config) Registry(name string) *Registry {
	for _, r := range c.Registries {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// resolve makes a file name relative to the configuration file absolute.
func (c *Config) resolve(name string) string {
	if name == "" || filepath.IsAbs(name) || c.path == "" {
		return name
	}
	return filepath.Join(filepath.Dir(c.path), name)
}

// Location returns the parsed URL of the registry.
func (r *Registry) Location() oci.Location {
	loc, _ := oci.ParseLocation(r.URL)
	return loc
}

// Options returns the connection options of the named registry.
func (c *Config) Options(r *Registry) *oci.Options {
	return &oci.Options{
		PlainHTTP:     r.PlainHTTP,
		CACertFile:    c.resolve(r.CACert),
		TLSSkipVerify: r.TLSSkipVerify,
	}
}

// Policy is an effective trust policy.
type Policy struct {
	RequireSignature bool
	// KeyFiles are the absolute file names of the trusted keys.
	KeyFiles []string
}

// TrustPolicy returns the trust policy for packs of the named registry. A
// registry's own trust block takes precedence over the default trust block:
// its public keys replace the default keys if it lists any, and its
// require_signature setting replaces the default if it is set.
func (c *Config) TrustPolicy(registry string) Policy {
	var p Policy
	if c.Trust != nil {
		if c.Trust.RequireSignature != nil {
			p.RequireSignature = *c.Trust.RequireSignature
		}
		p.KeyFiles = c.resolveAll(c.Trust.PublicKeys)
	}
	if r := c.Registry(registry); r != nil && r.Trust != nil {
		if r.Trust.RequireSignature != nil {
			p.RequireSignature = *r.Trust.RequireSignature
		}
		if len(r.Trust.PublicKeys) > 0 {
			p.KeyFiles = c.resolveAll(r.Trust.PublicKeys)
		}
	}
	return p
}

func (c *Config) resolveAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, c.resolve(n))
	}
	return out
}

// LoadKeys reads the trusted public keys of the policy.
func (p Policy) LoadKeys() ([]*signing.PublicKey, error) {
	var keys []*signing.PublicKey
	for _, f := range p.KeyFiles {
		k, err := signing.LoadPublicKey(f)
		if err != nil {
			return nil, fmt.Errorf("failed to load trusted key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// SaveRegistry adds the registry definition to the configuration file at
// path, replacing a definition with the same name. Other content of the file
// is preserved.
func SaveRegistry(path string, r *Registry) error {
	if err := r.Validate(); err != nil {
		return err
	}
	f, err := readForWrite(path)
	if err != nil {
		return err
	}
	body := f.Body()

	// Replace an existing definition in place, keeping its comments and
	// position; otherwise append a new block.
	var rb *hclwrite.Body
	for _, b := range body.Blocks() {
		if b.Type() == "registry" && len(b.Labels()) == 1 && b.Labels()[0] == r.Name {
			if rb != nil {
				body.RemoveBlock(b)
				continue
			}
			rb = b.Body()
			for name := range rb.Attributes() {
				rb.RemoveAttribute(name)
			}
			for _, nested := range rb.Blocks() {
				rb.RemoveBlock(nested)
			}
		}
	}
	if rb == nil {
		if len(body.Blocks()) > 0 || len(body.Attributes()) > 0 {
			body.AppendNewline()
		}
		rb = body.AppendNewBlock("registry", []string{r.Name}).Body()
	}
	rb.SetAttributeValue("url", cty.StringVal(r.URL))
	if r.PlainHTTP {
		rb.SetAttributeValue("plain_http", cty.True)
	}
	if r.CACert != "" {
		rb.SetAttributeValue("ca_cert", cty.StringVal(r.CACert))
	}
	if r.TLSSkipVerify {
		rb.SetAttributeValue("tls_skip_verify", cty.True)
	}
	if r.Trust != nil {
		rb.AppendNewline()
		tb := rb.AppendNewBlock("trust", nil).Body()
		if r.Trust.RequireSignature != nil {
			tb.SetAttributeValue("require_signature", cty.BoolVal(*r.Trust.RequireSignature))
		}
		if len(r.Trust.PublicKeys) > 0 {
			keys := make([]cty.Value, 0, len(r.Trust.PublicKeys))
			for _, k := range r.Trust.PublicKeys {
				keys = append(keys, cty.StringVal(k))
			}
			tb.SetAttributeValue("public_keys", cty.ListVal(keys))
		}
	}
	return write(path, f)
}

// RemoveRegistry removes the named registry definition from the
// configuration file at path. It reports whether a definition was removed.
func RemoveRegistry(path, name string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	f, err := readForWrite(path)
	if err != nil {
		return false, err
	}
	if !removeRegistryBlock(f.Body(), name) {
		return false, nil
	}
	return true, write(path, f)
}

func removeRegistryBlock(body *hclwrite.Body, name string) bool {
	removed := false
	for _, b := range body.Blocks() {
		if b.Type() == "registry" && len(b.Labels()) == 1 && b.Labels()[0] == name {
			body.RemoveBlock(b)
			removed = true
		}
	}
	return removed
}

func readForWrite(path string) (*hclwrite.File, error) {
	src, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return hclwrite.NewEmptyFile(), nil
	}
	if err != nil {
		return nil, err
	}
	f, diags := hclwrite.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse %s: %s", path, diags.Error())
	}
	return f, nil
}

func write(path string, f *hclwrite.File) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, hclwrite.Format(f.Bytes()), 0o600)
}

// RegistryNames returns the names of the configured registries, sorted.
func (c *Config) RegistryNames() []string {
	names := make([]string, 0, len(c.Registries))
	for _, r := range c.Registries {
		names = append(names, r.Name)
	}
	sort.Strings(names)
	return names
}
