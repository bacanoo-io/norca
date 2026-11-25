// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/internal/credstore"
	"github.com/bacanoo-io/norca/rigger/oci"
)

var (
	errPublishTo = errors.New("module publish needs -to oci://<host>/<repository>")
	errNoVersion = errors.New("a module is published with the version of its metadata; it has none")
	// errOnePublishDir is the error of a publish given more than one directory.
	errOnePublishDir = errors.New("module publish takes one directory")
)

// newPublishCommand is rigger module publish [dir] --to oci://host/repository,
// or rigger module publish [dir] [--key name.key], which signs and tags the
// module in its git repository. The directory may be before or after the
// flags.
func (a *app) newPublishCommand() *cobra.Command {
	var to, key string

	cmd := &cobra.Command{
		Use:   "publish [dir]",
		Short: "Publish a module: tag it in git, or push it to an OCI registry",
		Long: `Publish the module in dir (default: the current one) with the version of its
metadata.

Without --to: sign the module with the private key of --key (its password in
RIGGER_KEY_PASSWORD), commit the signature and tag the version in its git
repository. Nothing is pushed. The global registry lists only signed
versions.

With --to oci://<host>/<repository>: push the module, with its child modules
and signatures, tagged with its version.`,
		Example: `  rigger module publish --key release.key
  rigger module publish modules/web --to oci://registry.example.com/acme/web`,
		// An argument that is not read would publish another directory than
		// the one named.
		Args: oneDirectory(errOnePublishDir),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}

			if to == "" {
				return publishGit(cmd.Context(), a.logger, a.stdout, a.stderr, dir, key)
			}

			return publish(cmd.Context(), a.logger, a.stdout, a.stderr, dir, to)
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "the OCI repository: `oci://<host>/<repository>`; none: tag in git")
	cmd.Flags().StringVar(&key, keyFlag, "", "git: the private `key` the version is signed with (password in RIGGER_KEY_PASSWORD)")

	return cmd
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

	client := oci.NewClient()
	client.SetCredentials(credentials{kind: credstore.OCI}.oci)

	manifest, err := client.Push(ctx, repo, oci.Module{Name: md.Name, Version: md.Version, Path: m.Dir(), Digest: digest}, layer)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "published %s %s to %s:%s (%s)\n", md.Name, md.Version, to, md.Version, manifest)

	return err
}
