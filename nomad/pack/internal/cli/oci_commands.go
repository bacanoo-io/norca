// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package cli

import (
	stderrors "errors"
	"fmt"
	"os"
	"strings"

	"github.com/posener/complete"

	"github.com/hashicorp/nomad/pack/internal/pkg/flag"
	"github.com/hashicorp/nomad/pack/internal/pkg/loader"
	"github.com/hashicorp/nomad/pack/internal/pkg/oci"
	"github.com/hashicorp/nomad/pack/internal/pkg/packlock"
	"github.com/hashicorp/nomad/pack/internal/pkg/signing"
	"github.com/hashicorp/nomad/pack/sdk/pack"
)

// groupHelpCommand provides the top level help of a group of subcommands.
type groupHelpCommand struct {
	*baseCommand
	name        string
	synopsis    string
	subcommands string
}

func (c *groupHelpCommand) Run(args []string) int {
	c.cmdKey = c.name
	if err := c.Init(WithNoArgs(args), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.Info(fmt.Sprintf("The %s command requires one of the following subcommands: %s.", c.name, c.subcommands))
		return 1
	}
	c.ui.Info(fmt.Sprintf("The %s command requires one of the following subcommands: %s.", c.name, c.subcommands))
	return 0
}

func (c *groupHelpCommand) Flags() *flag.Sets                    { return c.flagSet(0, nil) }
func (c *groupHelpCommand) AutocompleteArgs() complete.Predictor { return complete.PredictNothing }
func (c *groupHelpCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *groupHelpCommand) Synopsis() string                     { return c.synopsis }
func (c *groupHelpCommand) Help() string {
	return formatHelp(fmt.Sprintf(`
	Usage: norca pack %s <subcommand> [options]

	%s

	Subcommands: %s.
`, c.name, c.synopsis, c.subcommands))
}

// loadPackForPublishing loads the pack in dir and checks that it can be
// published: it is valid, its lock file (if any) matches, and all its
// dependencies are vendored, so that it can be rendered without network
// access to the dependencies' sources.
func loadPackForPublishing(dir string) (*pack.Pack, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a pack directory", dir)
	}
	p, err := loader.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to load pack: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pack: %w", err)
	}
	if _, err := packlock.Verify(dir, p.Metadata); err != nil && !stderrors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := packlock.Generate(dir, p.Metadata, nil); err != nil {
		return nil, err
	}
	return p, nil
}

// PushCommand publishes a pack directory to an OCI registry.
type PushCommand struct {
	*baseCommand
	reg     registryFlags
	tag     string
	signKey string
}

func (c *PushCommand) Run(args []string) int {
	c.cmdKey = "push"
	if err := c.Init(
		WithCustomArgs(args, func(_ *baseCommand, a []string) error {
			if len(a) < 1 || len(a) > 2 {
				return fmt.Errorf("this command requires 1 or 2 args: <pack directory> [oci://host/repository/name[:tag]]")
			}
			return nil
		}),
		WithFlags(c.Flags()),
		WithNoConfig(),
		WithClient(false),
	); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}

	dir := c.args[0]
	p, err := loadPackForPublishing(dir)
	if err != nil {
		c.ui.ErrorWithContext(err, "pack can not be published")
		return 1
	}
	meta := p.Metadata

	tag := c.tag
	if tag == "" {
		tag = meta.Pack.Version
	}
	ref := meta.Pack.Name
	if len(c.args) == 2 {
		ref = c.args[1]
	} else if c.reg.registry == "" {
		c.ui.Error("Name the target with an oci:// reference or with --registry.")
		return 1
	}
	target, err := c.reg.resolveTarget(ref, c.tag)
	if err != nil {
		c.ui.ErrorWithContext(err, "invalid target")
		return 1
	}
	loc := target.endpoint.Location
	if loc.Reference != "" {
		tag = loc.Reference
	}
	if tag == "" || strings.Contains(tag, ":") {
		c.ui.Error("A version tag is required: set pack.version in metadata.hcl or use --tag.")
		return 1
	}
	if target.name != meta.Pack.Name {
		c.ui.Error(fmt.Sprintf("The repository %s must be named like the pack %q.", loc.RepositoryName(), meta.Pack.Name))
		return 1
	}

	a, err := oci.Build(dir, meta)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to package pack")
		return 1
	}
	repo, err := target.endpoint.Options.Repository(loc)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to connect to registry")
		return 1
	}
	if err := a.Push(c.Ctx, repo, tag); err != nil {
		c.ui.ErrorWithContext(err, "failed to push pack")
		return 1
	}
	c.ui.Success(fmt.Sprintf("Pushed %s", loc.WithReference(tag)))
	c.ui.Output(fmt.Sprintf("Digest: %s", a.Digest()))

	if c.signKey != "" {
		key, err := loadSigningKey(c.signKey)
		if err != nil {
			c.ui.ErrorWithContext(err, "failed to sign pack")
			return 1
		}
		if err := signArtifact(c.baseCommand, repo, loc, a.Digest(), key); err != nil {
			c.ui.ErrorWithContext(err, "failed to sign pack")
			return 1
		}
	}
	return 0
}

func (c *PushCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Push Options")
		f.StringVar(&flag.StringVar{
			Name:   "tag",
			Target: &c.tag,
			Usage:  `Tag (version) to publish the pack as. Defaults to pack.version of metadata.hcl.`,
		})
		f.StringVar(&flag.StringVar{
			Name:   "sign-key",
			Target: &c.signKey,
			Usage: fmt.Sprintf(`Sign the pushed pack with this cosign compatible private key.
					The key password is read from %s.`, EnvKeyPassword),
		})
		c.reg.addFlags(set.NewSet("Registry Options"))
	})
}

func (c *PushCommand) AutocompleteArgs() complete.Predictor { return complete.PredictDirs("*") }
func (c *PushCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *PushCommand) Synopsis() string                     { return "Publish a pack to an OCI registry" }
func (c *PushCommand) Help() string {
	c.Example = `
	# Publish a pack to a configured OCI registry, as version pack.version.
	norca pack push ./redis --registry=internal

	# Publish to an explicit repository and tag, and sign it.
	norca pack push ./redis oci://registry.example.com/packs/redis:0.2.1 --sign-key=cosign.key
	`
	return formatHelp(`
	Usage: norca pack push <pack directory> [oci://host/repository/name[:tag]] [options]

	Package a pack directory as an OCI artifact and push it to an OCI registry.
	The repository must be named like the pack, the tag is the pack version.
	The pack must be valid, its pack.lock (if any) must match, and its
	dependencies must be vendored.

` + c.GetExample() + c.Flags().Help())
}

// signArtifact signs the manifest digest and stores the signature next to the
// artifact in repo.
func signArtifact(c *baseCommand, repo oci.Target, loc oci.Location, digest string, key *signingKey) error {
	payload, err := signing.NewPayload(loc.RepositoryName(), digest)
	if err != nil {
		return err
	}
	sig, err := signing.Sign(key, payload)
	if err != nil {
		return err
	}
	if err := oci.AttachSignature(c.Ctx, repo, digest, signing.Signature{Payload: payload, Signature: sig}, key.Public()); err != nil {
		return err
	}
	c.ui.Success(fmt.Sprintf("Signed %s with key %s", loc.WithReference(digest), key.id))
	return nil
}

// SignCommand signs a pack in an OCI registry.
type SignCommand struct {
	*baseCommand
	reg registryFlags
	key string
}

func (c *SignCommand) Run(args []string) int {
	c.cmdKey = "sign"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	if c.key == "" {
		c.ui.Error("The --key flag is required.")
		return 1
	}
	target, err := c.reg.resolveTarget(c.args[0], "")
	if err != nil {
		c.ui.ErrorWithContext(err, "invalid pack reference")
		return 1
	}
	loc := target.endpoint.Location
	if loc.Reference == "" {
		c.ui.Error("A version (tag) or digest of the pack is required.")
		return 1
	}
	key, err := loadSigningKey(c.key)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to load key")
		return 1
	}
	repo, err := target.endpoint.Options.Repository(loc)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to connect to registry")
		return 1
	}
	// Fetching validates that the reference is a pack.
	a, err := oci.Fetch(c.Ctx, repo, loc.Reference)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to fetch pack")
		return 1
	}
	if err := signArtifact(c.baseCommand, repo, loc.WithReference(""), a.Digest(), key); err != nil {
		c.ui.ErrorWithContext(err, "failed to sign pack")
		return 1
	}
	return 0
}

func (c *SignCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Sign Options")
		f.StringVar(&flag.StringVar{
			Name:   "key",
			Target: &c.key,
			Usage: fmt.Sprintf(`Path to the cosign compatible private key to sign with. The
					key password is read from %s (or COSIGN_PASSWORD), or
					asked for interactively.`, EnvKeyPassword),
		})
		c.reg.addFlags(set.NewSet("Registry Options"))
	})
}

func (c *SignCommand) AutocompleteArgs() complete.Predictor { return complete.PredictNothing }
func (c *SignCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *SignCommand) Synopsis() string                     { return "Sign a pack in an OCI registry" }
func (c *SignCommand) Help() string {
	c.Example = `
	# Sign version 0.2.1 of the redis pack in a configured registry.
	norca pack sign --key=cosign.key --registry=internal redis:0.2.1

	# Sign by explicit reference.
	norca pack sign --key=cosign.key oci://registry.example.com/packs/redis:0.2.1
	`
	return formatHelp(`
	Usage: norca pack sign --key=<private key> <pack reference> [options]

	Sign a pack in an OCI registry with a cosign compatible key pair (see
	"norca pack key generate"). The signature is stored in the registry next to
	the pack in the format cosign uses, so it can also be checked with
	"cosign verify --key cosign.pub --insecure-ignore-tlog <reference>".

` + c.GetExample() + c.Flags().Help())
}

// VerifyCommand verifies the signature of a pack in an OCI registry.
type VerifyCommand struct {
	*baseCommand
	reg  registryFlags
	keys []string
}

func (c *VerifyCommand) Run(args []string) int {
	c.cmdKey = "verify"
	if err := c.Init(WithExactArgs(1, args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	target, err := c.reg.resolveTarget(c.args[0], "")
	if err != nil {
		c.ui.ErrorWithContext(err, "invalid pack reference")
		return 1
	}
	ep := target.endpoint
	keyFiles := c.keys
	if len(keyFiles) == 0 {
		keyFiles = ep.Policy.KeyFiles
	}
	if len(keyFiles) == 0 {
		c.ui.Error("No public keys: use --key or configure trusted keys for the registry.")
		return 1
	}
	var keys []*signing.PublicKey
	for _, f := range keyFiles {
		k, err := signing.LoadPublicKey(f)
		if err != nil {
			c.ui.ErrorWithContext(err, "failed to load public key")
			return 1
		}
		keys = append(keys, k)
	}

	repo, err := ep.Options.Repository(ep.Location)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to connect to registry")
		return 1
	}
	ref := ep.Location.Reference
	if ref == "" {
		ref = "latest"
	}
	a, err := oci.Fetch(c.Ctx, repo, ref)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to fetch pack")
		return 1
	}
	sigs, err := oci.FetchSignatures(c.Ctx, repo, a.Digest())
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to fetch signatures")
		return 1
	}
	res := signing.Verify(a.Digest(), sigs, keys)
	c.ui.Output(fmt.Sprintf("Pack:     %s %s", a.Config.Name, a.Config.Version))
	c.ui.Output(fmt.Sprintf("Digest:   %s", a.Digest()))
	c.ui.Output(fmt.Sprintf("Content:  %s", a.TreeDigest()))
	if !res.Verified {
		c.ui.Error(fmt.Sprintf("No valid signature by a trusted key (%d signature(s) found).", len(sigs)))
		return 1
	}
	c.ui.Output(fmt.Sprintf("Signer:   %s (%s)", res.KeyID, res.KeySource))
	c.ui.Success("Signature verified")
	return 0
}

func (c *VerifyCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Verify Options")
		f.StringSliceVar(&flag.StringSliceVar{
			Name:   "key",
			Target: &c.keys,
			Usage: `Path to a trusted public key. Can be repeated. Defaults to the
					trusted keys of the registry's trust policy.`,
		})
		c.reg.addFlags(set.NewSet("Registry Options"))
	})
}

func (c *VerifyCommand) AutocompleteArgs() complete.Predictor { return complete.PredictNothing }
func (c *VerifyCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *VerifyCommand) Synopsis() string                     { return "Verify the signature of a pack in an OCI registry" }
func (c *VerifyCommand) Help() string {
	c.Example = `
	norca pack verify --key=cosign.pub --registry=internal redis:0.2.1
	norca pack verify --key=cosign.pub oci://registry.example.com/packs/redis@sha256:...
	`
	return formatHelp(`
	Usage: norca pack verify [--key=<public key>]... <pack reference> [options]

	Verify that a pack in an OCI registry is signed by a trusted key, and print
	its digest and the signer's key ID. Exits with 1 if no valid signature is
	found.

` + c.GetExample() + c.Flags().Help())
}

// KeyGenerateCommand creates a cosign compatible signing key pair.
type KeyGenerateCommand struct {
	*baseCommand
	prefix string
	force  bool
}

func (c *KeyGenerateCommand) Run(args []string) int {
	c.cmdKey = "key generate"
	if err := c.Init(WithNoArgs(args), WithFlags(c.Flags()), WithNoConfig(), WithClient(false)); err != nil {
		c.ui.ErrorWithContext(err, ErrParsingArgsOrFlags)
		c.ui.Info(c.helpUsageMessage())
		return 1
	}
	privFile, pubFile := c.prefix+".key", c.prefix+".pub"
	if !c.force {
		for _, f := range []string{privFile, pubFile} {
			if _, err := os.Stat(f); err == nil {
				c.ui.Error(fmt.Sprintf("%s already exists; use --force to overwrite it.", f))
				return 1
			}
		}
	}
	pw, err := keyPassword(true)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to read password")
		return 1
	}
	kp, err := signing.GenerateKeyPair(pw)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to generate key pair")
		return 1
	}
	if err := os.WriteFile(privFile, kp.PrivateKeyPEM, 0o600); err != nil {
		c.ui.ErrorWithContext(err, "failed to write private key")
		return 1
	}
	if err := os.WriteFile(pubFile, kp.PublicKeyPEM, 0o644); err != nil {
		c.ui.ErrorWithContext(err, "failed to write public key")
		return 1
	}
	pub, err := signing.ParsePublicKey(kp.PublicKeyPEM)
	if err != nil {
		c.ui.ErrorWithContext(err, "failed to read public key")
		return 1
	}
	id, _ := signing.KeyID(pub)
	c.ui.Success(fmt.Sprintf("Private key written to %s", privFile))
	c.ui.Success(fmt.Sprintf("Public key written to %s", pubFile))
	c.ui.Output(fmt.Sprintf("Key ID: %s", id))
	return 0
}

func (c *KeyGenerateCommand) Flags() *flag.Sets {
	return c.flagSet(0, func(set *flag.Sets) {
		f := set.NewSet("Key Options")
		f.StringVar(&flag.StringVar{
			Name:    "output-prefix",
			Target:  &c.prefix,
			Default: "cosign",
			Usage:   `Prefix of the key files: <prefix>.key and <prefix>.pub.`,
		})
		f.BoolVar(&flag.BoolVar{
			Name:   "force",
			Target: &c.force,
			Usage:  `Overwrite existing key files.`,
		})
	})
}

func (c *KeyGenerateCommand) AutocompleteArgs() complete.Predictor { return complete.PredictNothing }
func (c *KeyGenerateCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }
func (c *KeyGenerateCommand) Synopsis() string                     { return "Generate a pack signing key pair" }
func (c *KeyGenerateCommand) Help() string {
	return formatHelp(`
	Usage: norca pack key generate [options]

	Generate an ECDSA P-256 key pair for signing packs, in the format of
	"cosign generate-key-pair": an encrypted private key (<prefix>.key) and a
	public key (<prefix>.pub). Keys generated by cosign can be used as well.
	The password is read from ` + EnvKeyPassword + ` (or COSIGN_PASSWORD), or
	asked for interactively. Keep the private key secret; distribute the
	public key to the clients that verify packs.

` + c.Flags().Help())
}
