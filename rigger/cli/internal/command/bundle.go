// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca/rigger/cli/internal/bundle"
)

var errBundleOut = errors.New("bundle create needs -o, the file to write")

// runBundle runs rigger bundle create|inspect|import (design 7.1, 10).
func runBundle(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "bundle needs a command: create, inspect or import")

		return exitError
	}

	cmd, args := args[0], args[1:]

	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}

	flags := flag.NewFlagSet("rigger bundle "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("o", "", "create: the bundle to write (platform.riggerbundle)")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	positional = append(positional, flags.Args()...)
	arg := "."

	if len(positional) > 0 {
		arg = positional[0]
	}

	return run(ctx, logger, stderr, "bundle."+cmd, func(ctx context.Context) error {
		switch cmd {
		case "create":
			return createBundle(ctx, logger, stdout, stderr, arg, *out)
		case "inspect":
			return inspectBundle(stdout, arg)
		case "import":
			return importBundle(ctx, logger, stdout, arg)
		default:
			return fmt.Errorf("unknown bundle command %q", cmd)
		}
	})
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

// importBundle writes the modules of a bundle into the cache and checks
// each against the digest of the manifest.
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

	for _, mod := range m.Modules {
		dir := fetcher.BundleDir(mod.Commit)
		if err := bundle.WriteTree(dir, files[mod.Commit]); err != nil {
			return err
		}

		digest, err := eng.ModuleDigest(ctx, os.DirFS(dir), mod.Path)
		if err != nil {
			return fmt.Errorf("%s %s: %w", mod.Source, mod.Version, err)
		}

		if digest != mod.Digest {
			return fmt.Errorf("%s %s: the bundle holds content of the digest %s, its manifest says %s", mod.Source, mod.Version, digest, mod.Digest)
		}

		if _, err := fmt.Fprintf(w, "imported %s %s (%s)\n", mod.Source, mod.Version, digest); err != nil {
			return err
		}
	}

	return nil
}
