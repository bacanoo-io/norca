// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/bacanoo-io/norca/rigger/cli/internal/oci"
)

var (
	errPublishTo = errors.New("module publish needs -to oci://<host>/<repository>")
	errNoVersion = errors.New("a module is published with the version of its metadata; it has none")
)

// runPublish runs rig module publish [dir] -to oci://host/repository, or
// rig module publish [dir] [-key name.key], which signs and tags the
// module in its git repository.
func runPublish(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}

	flags := flag.NewFlagSet("rig module publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	to := flags.String("to", "", "the OCI repository: oci://<host>/<repository>; none: tag in git")
	key := flags.String("key", "", "git: the private key the version is signed with (password in RIG_KEY_PASSWORD)")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	return run(ctx, logger, stderr, "module.publish", func(ctx context.Context) error {
		if *to == "" {
			return publishGit(ctx, logger, stdout, stderr, dir, *key)
		}

		return publish(ctx, logger, stdout, stderr, dir, *to)
	})
}

// publish pushes the module in dir to an OCI repository, tagged with its
// version: its directories and those of its child modules, signatures
// included.
func publish(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, to string) error {
	repo, ok := strings.CutPrefix(to, "oci://")
	if !ok || !strings.Contains(repo, "/") {
		return errPublishTo
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	md := m.Metadata()
	if md.Version == "" {
		return errNoVersion
	}

	digest, err := m.Digest()
	if err != nil {
		return err
	}

	root, dirs := m.Files()

	layer, err := oci.Layer(root, dirs)
	if err != nil {
		return err
	}

	manifest, err := oci.NewClient().Push(ctx, repo, oci.Module{Name: md.Name, Version: md.Version, Path: m.Dir(), Digest: digest}, layer)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "published %s %s to %s:%s (%s)\n", md.Name, md.Version, to, md.Version, manifest)

	return err
}
