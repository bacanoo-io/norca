// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-rig/sign"
)

var (
	errNotGit    = errors.New("module publish without -to tags the module in its git repository; it is in none")
	errDirty     = errors.New("the module has changes that are not committed")
	errTagExists = errors.New("the version is tagged already; a tag is never moved, publish a new version")
	errOutside   = errors.New("the module is not in the repository")
)

// publishGit publishes the module in dir in its git repository (design
// 7.2, 7.3): it validates the module, signs its tree digest with key into
// .rig-signatures and commits the signature, and tags the commit with the
// tag of the version: v<version> for a module at the root of the
// repository, <name>/v<version> for one in a directory. The tag is not
// pushed; the global registry lists it from the repository once it is.
func publishGit(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string) error {
	top, rel, err := repositoryOf(ctx, dir)
	if err != nil {
		return err
	}

	if err := validateModule(ctx, logger, io.Discard, stderr, dir); err != nil {
		return err
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	m, _, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	md := m.Metadata()
	if md.Version == "" {
		return errNoVersion
	}

	tag := "v" + md.Version
	if rel != "." {
		tag = path.Base(rel) + "/v" + md.Version
	}

	if err := publishable(ctx, top, rel, tag); err != nil {
		return err
	}

	if keyFile == "" {
		warn(stderr, "%s %s is not signed (-key): the global registry lists only signed versions", md.Name, md.Version)
	} else if err := commitSignature(ctx, logger, stdout, stderr, top, rel, dir, keyFile, md.Name+": sign "+md.Version); err != nil {
		return err
	}

	if _, err := gitOut(ctx, top, "tag", "-a", "-m", md.Name+" "+md.Version, tag); err != nil {
		return err
	}

	commit, err := gitOut(ctx, top, "rev-parse", tag+"^{commit}")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "tagged %s %s as %s at %s; push it: git push origin %s\n", md.Name, md.Version, tag, commit, tag)

	return err
}

// publishable checks that a version is not tagged yet and that its module
// has no changes that are not committed.
func publishable(ctx context.Context, top, rel, tag string) error {
	if _, err := gitOut(ctx, top, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag); err == nil {
		return fmt.Errorf("%s: %w", tag, errTagExists)
	}

	if status, err := gitOut(ctx, top, "status", "--porcelain", "--", rel); err != nil || status != "" {
		return fmt.Errorf("%w:\n%s", errDirty, status)
	}

	return nil
}

// repositoryOf returns the top of the git repository dir is in and the
// directory of dir in it, slash separated.
func repositoryOf(ctx context.Context, dir string) (top, rel string, err error) {
	top, err = gitOut(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", errNotGit
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}

	if top, err = filepath.EvalSymlinks(top); err != nil {
		return "", "", err
	}

	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return "", "", err
	}

	rel, err = filepath.Rel(top, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", errOutside
	}

	return top, filepath.ToSlash(rel), nil
}

// commitSignature signs the module in dir and commits its signature, if it
// is new.
func commitSignature(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, top, rel, dir, keyFile, msg string) error {
	if err := signModule(ctx, logger, stdout, stderr, dir, keyFile); err != nil {
		return err
	}

	sigDir := path.Join(rel, sign.SignatureDir)
	if _, err := gitOut(ctx, top, "add", "--", sigDir); err != nil {
		return err
	}

	if staged, _ := gitOut(ctx, top, "diff", "--cached", "--name-only", "--", sigDir); staged == "" {
		return nil
	}

	_, err := gitOut(ctx, top, "commit", "--quiet", "-m", msg, "--", sigDir)

	return err
}

// gitOut runs git in dir and returns its output, trimmed.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: git of the module's repository
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}
