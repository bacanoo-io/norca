// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	"crypto"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/posener/complete"

	"github.com/hashicorp/nomad/pack/internal/pkg/bundle"
	"github.com/hashicorp/nomad/pack/internal/pkg/caching"
	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packconfig"
	"github.com/hashicorp/nomad/pack/internal/pkg/supplychain"
	"github.com/hashicorp/nomad/pack/terminal"
)

// BundleCreateCommand creates an offline bundle of packs.
type BundleCreateCommand struct {
	*baseCommand
	reg     registryFlags
	output  string
	signKey string
}

func (c *BundleCreateCommand) Run(args []string) int {
	c.cmdKey = "bundle create"
	if err := c.Init(WithMinimumNArgs(1, args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	if c.output == "" {
		c.ui.Error("The --output flag is required.")
		return 1
	}
	if err := c.create(); err != nil {
		c.ui.ErrorWithContext(err, "failed to create bundle")
		return 1
	}
	return 0
}

func (c *BundleCreateCommand) create() error {
	conf, err := packconfig.LoadDefault()
	if err != nil {
		return err
	}
	b, err := bundle.NewBuilder()
	if err != nil {
		return err
	}
	defer b.Close()

	var key *signingKey
	if c.signKey != "" {
		if key, err = loadSigningKey(c.signKey); err != nil {
			return err
		}
	}

	open := func(loc oci.Location) (supplychain.Source, error) {
		ep := supplychain.EndpointFor(conf, loc)
		return ep.Options.Repository(loc)
	}

	for _, arg := range c.args {
		if info, err := os.Stat(arg); err == nil && info.IsDir() {
			p, err := loadPackForPublishing(arg)
			if err != nil {
				return fmt.Errorf("%s: %w", arg, err)
			}
			a, err := oci.Build(arg, p.Metadata)
			if err != nil {
				return err
			}
			if p.Metadata.Pack.Version == "" {
				return fmt.Errorf("%s: pack.version must be set in metadata.hcl", arg)
			}
			// A nil *signingKey must not become a non-nil interface.
			var signer crypto.Signer
			if key != nil {
				signer = key
			}
			e, err := b.AddArtifact(c.Ctx, a, p.Metadata.Pack.Version, "dir:"+filepath.Base(arg), signer)
			if err != nil {
				return err
			}
			c.ui.Info(fmt.Sprintf("Added %s %s (%s) from %s", e.Name, e.Tag, e.Digest, arg))
			if _, err := b.AddDependencies(c.Ctx, a, open); err != nil {
				return err
			}
			continue
		}

		target, err := c.reg.resolveTarget(arg, "")
		if err != nil {
			return err
		}
		ep := target.endpoint
		repo, err := ep.Options.Repository(ep.Location)
		if err != nil {
			return err
		}
		tag := ep.Location.Reference
		if tag == "" {
			if tag, err = supplychain.ResolveTag(c.Ctx, repo, ""); err != nil {
				return fmt.Errorf("%s: %w", arg, err)
			}
		}
		e, a, _, err := b.Add(c.Ctx, repo, tag, ep.Location.WithReference(tag).String())
		if err != nil {
			return fmt.Errorf("%s: %w", arg, err)
		}
		sigs, err := oci.FetchSignatures(c.Ctx, repo, a.Digest())
		if err != nil {
			return err
		}
		if _, err := supplychain.ApplyPolicy(ep.Policy, a.Digest(), sigs); err != nil {
			return fmt.Errorf("%s: %w", arg, err)
		}
		c.ui.Info(fmt.Sprintf("Added %s %s (%s) from %s", e.Name, e.Tag, e.Digest, e.Source))
		other, err := b.AddDependencies(c.Ctx, a, open)
		if err != nil {
			return err
		}
		if err := checkVendored(a, other); err != nil {
			return err
		}
	}

	for _, e := range b.Index().Packs {
		if len(e.DependencyOf) > 0 {
			c.ui.Info(fmt.Sprintf("Added dependency %s %s (%s) of %s", e.Name, e.Tag, e.Digest, strings.Join(e.DependencyOf, ", ")))
		}
	}
	if err := b.WriteFile(c.output); err != nil {
		return err
	}
	c.ui.Success(fmt.Sprintf("Wrote bundle %s with %d pack(s)", c.output, len(b.Index().Packs)))
	return nil
}

// checkVendored fails if dependencies that are not available from an OCI
// registry are not vendored in the pack, since such a pack can not be rendered
// offline.
func checkVendored(a *oci.Artifact, deps []oci.DependencyRef) error {
	if len(deps) == 0 {
		return nil
	}
	tmp, err := os.MkdirTemp("", "norca-bundle-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "pack")
	if err := a.Extract(dir); err != nil {
		return err
	}
	for _, d := range deps {
		if _, err := os.Stat(filepath.Join(dir, "deps", d.Name)); err != nil {
			return fmt.Errorf("pack %s: dependency %q (%s) is not vendored and can not be rendered offline; "+
				"vendor it with \"norca pack deps vendor\" before publishing", a.Config.Name, d.Name, d.Source)
		}
	}
	return nil
}

func (c *BundleCreateCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Bundle Options")
		f.StringVarP(&flag.StringVarP{
			StringVar: &flag.StringVar{
				Name:   "output",
				Target: &c.output,
				Usage:  `File name of the bundle to write.`,
			},
			Shorthand: "o",
		})
		f.StringVar(&flag.StringVar{
			Name:   "sign-key",
			Target: &c.signKey,
			Usage: `Sign packs added from local directories with this cosign
					compatible private key.`,
		})
		c.reg.addFlags(set.NewSet("Registry Options"))
	})
}

func (c *BundleCreateCommand) AutocompleteArgs() complete.Predictor { return complete.PredictDirs("*") }
func (c *BundleCreateCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *BundleCreateCommand) Synopsis() string {
	return "Create an offline bundle of packs, dependencies and signatures"
}
func (c *BundleCreateCommand) Help() string {
	c.Example = `
	# Bundle two packs of a configured registry, with their dependencies and
	# signatures.
	norca pack bundle create --output=packs.tar --registry=internal redis:0.2.1 postgres

	# Bundle a pack from a local directory and sign it.
	norca pack bundle create --output=packs.tar --sign-key=cosign.key ./redis
	`
	return formatHelp(`
	Usage: norca pack bundle create --output=<file> <pack>... [options]

	Create a bundle for moving packs into an environment without access to
	their registries (air-gapped installation). Every pack is either a
	reference to a pack in an OCI registry (oci://... or name[:version] with
	--registry) or a local pack directory. Dependencies with oci:// sources are
	added as well; other dependencies must be vendored into the pack. The
	signatures of the packs are included, and the bundle is verified with the
	trust policy of the target when it is imported.

` + c.GetExample() + c.Flags().Help())
}

// BundleImportCommand imports a bundle into the cache or an OCI registry.
type BundleImportCommand struct {
	*baseCommand
	reg     registryFlags
	pushTo  string
	cacheAs string
}

func (c *BundleImportCommand) Run(args []string) int {
	c.cmdKey = "bundle import"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	if c.reg.registry == "" && c.pushTo == "" && c.cacheAs == "" {
		c.ui.Error("Name a target: --registry or --push-to to push the packs into an OCI registry, --cache-as to import them into the local cache.")
		return 1
	}
	if err := c.importBundle(); err != nil {
		c.ui.ErrorWithContext(err, "failed to import bundle")
		return 1
	}
	return 0
}

func (c *BundleImportCommand) importBundle() error {
	conf, err := packconfig.LoadDefault()
	if err != nil {
		return err
	}
	b, err := bundle.Open(c.args[0])
	if err != nil {
		return err
	}
	defer b.Close()

	report := func(results []*supplychain.Result) {
		for _, r := range results {
			msg := fmt.Sprintf("%s (%s)", r.Source, r.Digest)
			if id := r.SignerKeyID(); id != "" {
				msg += ", signed by " + id
			}
			c.ui.Info(msg)
		}
	}

	if c.reg.registry != "" || c.pushTo != "" {
		var base oci.Location
		var opts *oci.Options
		var policy packconfig.Policy
		if c.pushTo != "" {
			if base, err = oci.ParseLocation(c.pushTo); err != nil {
				return err
			}
			ep := supplychain.EndpointFor(conf, base)
			opts, policy = ep.Options, ep.Policy
		} else {
			reg := conf.Registry(c.reg.registry)
			if reg == nil {
				return fmt.Errorf("OCI registry %q is not configured in %s", c.reg.registry, conf.Path())
			}
			base, opts, policy = reg.Location(), conf.Options(reg), conf.TrustPolicy(reg.Name)
		}
		c.reg.apply(opts)
		results, err := b.Push(c.Ctx, base, opts, policy)
		report(results)
		if err != nil {
			return err
		}
		c.ui.Success(fmt.Sprintf("Pushed %d pack(s) to %s", len(results), base))
	}

	if c.cacheAs != "" {
		results, err := b.ImportToCache(c.Ctx, caching.DefaultCachePath(), c.cacheAs, conf.TrustPolicy(c.cacheAs))
		report(results)
		if err != nil {
			return err
		}
		c.ui.Success(fmt.Sprintf("Imported %d pack(s) into the cache as registry %q; run them with --registry=%s", len(results), c.cacheAs, c.cacheAs))
	}
	return nil
}

func (c *BundleImportCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Import Options")
		f.StringVar(&flag.StringVar{
			Name:   "push-to",
			Target: &c.pushTo,
			Usage: `Push the packs into repositories below this oci:// location
					instead of a configured registry (--registry).`,
		})
		f.StringVar(&flag.StringVar{
			Name:   "cache-as",
			Target: &c.cacheAs,
			Usage: `Import the packs into the local pack cache under this
					registry name, so they can be run without any registry.`,
		})
		c.reg.addFlags(set.NewSet("Registry Options"))
	})
}

func (c *BundleImportCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFiles("*")
}
func (c *BundleImportCommand) AutocompleteFlags() complete.Flags { return c.Flags().Completions() }
func (c *BundleImportCommand) Synopsis() string {
	return "Import an offline bundle into the cache or an OCI registry"
}
func (c *BundleImportCommand) Help() string {
	c.Example = `
	# Push the packs of a bundle into the internal OCI registry.
	norca pack bundle import packs.tar --registry=internal

	# Import the packs into the local cache and run one of them.
	norca pack bundle import packs.tar --cache-as=offline
	norca pack run redis --registry=offline --ref=0.2.1
	`
	return formatHelp(`
	Usage: norca pack bundle import <bundle> [options]

	Import a bundle created with "norca pack bundle create". Every pack is
	verified with the trust policy of the target (the registry pushed to, or
	the cache registry name) before it is imported; packs are pushed together
	with their signatures.

` + c.GetExample() + c.Flags().Help())
}

// BundleInspectCommand lists the content of a bundle.
type BundleInspectCommand struct {
	*baseCommand
}

func (c *BundleInspectCommand) Run(args []string) int {
	c.cmdKey = "bundle inspect"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	idx, err := bundle.ReadIndex(c.args[0])
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to read bundle")
		return 1
	}
	table := terminal.NewTable("PACK NAME", "VERSION", "DIGEST", "SIGNED", "DEPENDENCY OF", "SOURCE")
	for _, e := range idx.Packs {
		table.Rows = append(table.Rows, []string{
			e.Name, e.Tag, e.Digest, fmt.Sprintf("%t", e.Signed), strings.Join(e.DependencyOf, ","), e.Source,
		})
	}
	c.ui.Table(table)
	return 0
}

func (c *BundleInspectCommand) Flags() *flag.Sets { return c.flagSet(0, nil) }
func (c *BundleInspectCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictFiles("*")
}
func (c *BundleInspectCommand) AutocompleteFlags() complete.Flags { return c.Flags().Completions() }
func (c *BundleInspectCommand) Synopsis() string                  { return "List the packs of an offline bundle" }
func (c *BundleInspectCommand) Help() string {
	return formatHelp(`
	Usage: norca pack bundle inspect <bundle>

	List the packs of a bundle with their versions, digests and signatures.
`)
}
