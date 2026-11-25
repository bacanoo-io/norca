// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/bundle"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

var errBundleOut = errors.New("bundle create needs -o, the file to write")

// cmdCreate is rigger bundle create.
const cmdCreate = "create"

// newBundleCommand is rigger bundle create|inspect|import (design 7.1, 10).
func (a *app) newBundleCommand() *cobra.Command {
	return newGroup(&cobra.Command{
		Use:   "bundle",
		Short: "Bundles for a cluster without a network: create, inspect, import",
		Long: `A bundle holds the modules of a stack in one file, for an install without a
network: created where the sources are reachable, imported where they are
not.`,
	},
		a.newBundleSubcommand("create [stack]", "Write the modules of a stack into a bundle",
			`Write the modules of the stack in the directory stack (default: the current
one), from their git sources as its lock file pins them, into the bundle of
--output.`,
			`  rigger bundle create stacks/platform -o platform.riggerbundle`),
		a.newBundleSubcommand("inspect <file>", "Show what a bundle holds",
			`Show the stack a bundle was made of and the modules it holds, with their
versions and digests.`,
			`  rigger bundle inspect platform.riggerbundle`),
		a.newBundleSubcommand("import <file>", "Put the modules of a bundle into the cache, or the cluster",
			`Check each module of a bundle against the digest its manifest names and put
them into the cache, all or none, for an install without a network. With
--server (or RIGGER_SERVER=1) the bundle goes into the cluster, for its
servers.`,
			`  rigger bundle import platform.riggerbundle
  rigger bundle import platform.riggerbundle --server`),
	)
}

// newBundleSubcommand is one command of rigger bundle. Each parses the
// flags they all took before and shows its own.
func (a *app) newBundleSubcommand(use, short, long, example string) *cobra.Command {
	var (
		out    string
		server bool
	)

	name, _, _ := strings.Cut(use, " ")

	cmd := &cobra.Command{
		Use: use, Short: short, Long: long, Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			if server && name != "import" {
				return errServerOnly
			}

			arg := "."
			if len(args) > 0 {
				arg = args[0]
			}

			switch name {
			case cmdCreate:
				return createBundle(cmd.Context(), a.logger, a.stdout, a.stderr, arg, out)
			case "inspect":
				return inspectBundle(a.stdout, arg)
			default:
				if server {
					return serverImportBundle(cmd.Context(), a.stdout, arg)
				}

				return importBundle(cmd.Context(), a.logger, a.stdout, arg)
			}
		},
	}

	cmd.Flags().StringVarP(&out, "output", "o", "", "the bundle to write (`platform.riggerbundle`)")
	cmd.Flags().BoolVar(&server, serverFlag, serverDefault() && name == "import", serverUsage)

	switch name {
	case cmdCreate:
		showOnly(cmd, "output")
	case "import":
		showOnly(cmd, serverFlag)
	default:
		showOnly(cmd)
	}

	return cmd
}

// createBundle writes a bundle of the modules of the stack in dir, as its
// lock file pins them.
func createBundle(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, out string) error {
	if out == "" {
		return errBundleOut
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	s, _, diags, err := loadStack(ctx, eng, &stackFlags{dir: dir})
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	var buf bytes.Buffer
	if err := bundle.Write(&buf, s.Name(), s.SourcedModules()); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Clean(out), buf.Bytes(), 0o644); err != nil { //nolint:gosec // G306: a bundle is the public content of modules
		return err
	}

	_, err = fmt.Fprintf(stdout, "wrote %s: %d module(s) of the stack %s\n", out, len(s.SourcedModules()), s.Name())

	return err
}

// readBundle reads a bundle file.
func readBundle(p string) (*bundle.Manifest, map[string]map[string][]byte, error) {
	f, err := os.Open(filepath.Clean(p))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close() //nolint:errcheck // read only

	return bundle.Read(f)
}

// inspectBundle writes what a bundle holds.
func inspectBundle(w io.Writer, p string) error {
	m, _, err := readBundle(p)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "bundle of the stack %s\n", m.Stack); err != nil {
		return err
	}

	for _, mod := range m.Modules {
		signed := ""
		if len(mod.Signers) > 0 {
			signed = ", signed by " + strings.Join(mod.Signers, ", ")
		}

		if _, err := fmt.Fprintf(w, "  %s %s (%s, %s)%s\n", mod.Source, mod.Version, mod.Ref, mod.Digest, signed); err != nil {
			return err
		}
	}

	return nil
}

// importBundle checks each module of a bundle against the digest of the
// manifest, then puts the modules into the cache: all of them, or none.
func importBundle(ctx context.Context, logger *slog.Logger, w io.Writer, p string) error {
	m, files, err := readBundle(p)
	if err != nil {
		return err
	}

	fetcher, err := newFetcher()
	if err != nil {
		return err
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	if len(m.Modules) == 0 {
		return nil
	}

	// Every module is checked in a directory beside the trees of the cache
	// first; only a bundle whose modules all have their digests is placed,
	// so a wrong one leaves nothing in the cache.
	// Above the key of the module, the commit and the key of the repository.
	bundles := filepath.Dir(filepath.Dir(filepath.Dir(fetcher.BundleDir(m.Modules[0].Repository, m.Modules[0].Commit, "", ""))))
	if err := os.MkdirAll(bundles, 0o750); err != nil {
		return err
	}

	staging, err := os.MkdirTemp(bundles, ".import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) //nolint:errcheck // what was not placed goes

	// A cache of its own, so that each repository of a commit has its tree.
	staged := fetch.New(staging)

	if err := checkBundle(ctx, eng, staged, m, files); err != nil {
		return err
	}

	for _, mod := range m.Modules {
		checked, dir := staged.BundleDir(mod.Repository, mod.Commit, mod.Path, mod.Digest), fetcher.BundleDir(mod.Repository, mod.Commit, mod.Path, mod.Digest)
		if err := placeTree(checked, dir); err != nil {
			return err
		}

		if _, err := fmt.Fprintf(w, "imported %s %s (%s)\n", mod.Source, mod.Version, mod.Digest); err != nil {
			return err
		}
	}

	return nil
}

// checkBundle writes the tree of each module of a bundle into the staging
// cache and checks that each module has the digest its manifest says.
func checkBundle(ctx context.Context, eng *rigger.Engine, staged *fetch.Fetcher, m *bundle.Manifest, files map[string]map[string][]byte) error {
	for _, mod := range m.Modules {
		dir := staged.BundleDir(mod.Repository, mod.Commit, mod.Path, mod.Digest)
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			if err := bundle.WriteTree(dir, bundle.ModuleFiles(mod, files)); err != nil {
				return err
			}
		}

		digest, err := eng.SourcedDigest(ctx, os.DirFS(dir), mod)
		if err != nil {
			return fmt.Errorf("%s %s: %w", mod.Source, mod.Version, err)
		}

		if digest != mod.Digest {
			return fmt.Errorf("%s %s: the bundle holds content of the digest %s, its manifest says %s", mod.Source, mod.Version, digest, mod.Digest)
		}
	}

	return nil
}

// placeTree moves a checked tree to dir. A tree already at dir is kept, and
// a module the bundle names twice is done.
func placeTree(checked, dir string) error {
	if _, err := os.Stat(checked); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}

	if err := os.Rename(checked, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}

		return err
	}

	return nil
}
