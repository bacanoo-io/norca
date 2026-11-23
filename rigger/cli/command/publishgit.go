// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/sign"
)

var (
	errNotGit    = errors.New("module publish without -to tags the module in its git repository; it is in none")
	errDirty     = errors.New("the module has changes that are not committed")
	errTagExists = errors.New("the version is tagged already; a tag is never moved, publish a new version")
	errOutside   = errors.New("the module is not in the repository")
	// errCommitted follows the diagnostics of the files of the commit.
	errCommitted = errors.New("the files on disk are valid, those of the commit are not: a file the module needs is not committed " +
		"(git status --ignored shows the files that git ignores); nothing is tagged")
	errSignedTree = errors.New("the commit of the signature changed more of the version than its signatures; nothing is tagged")
)

// publishGit publishes the module in dir in its git repository (design
// 7.2, 7.3): it validates the module, signs the tree digest of its commit
// with key into .rigger-signatures and commits the signature, and tags the
// commit with the tag of the version: v<version> for a module at the root of
// the repository, <name>/v<version> for one in a directory. The tag is not
// pushed; the global registry lists it from the repository once it is.
func publishGit(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string) error {
	if err := validateModule(ctx, logger, io.Discard, stderr, dir); err != nil {
		return err
	}

	return tagVersion(ctx, logger, stdout, stderr, dir, keyFile, false)
}

// publishStack publishes the stack in a directory as publishGit a module
// (design 7.5): it validates the stack with its lock file, which must pin
// every module of a source, then signs and tags it.
func publishStack(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, f *stackFlags) error {
	if err := validateStack(ctx, logger, io.Discard, stderr, f); err != nil {
		return err
	}

	return tagVersion(ctx, logger, stdout, stderr, f.dir, f.key, true)
}

// tagVersion signs the module or stack in dir with key into .rigger-signatures
// and commits the signature, then tags the commit with the tag of the
// version: v<version> at the root of the repository, <name>/v<version> in a
// directory. The tag is not pushed.
//
// What is signed and tagged is the commit of HEAD, read as a registry reads
// the tree of the tag (readHead), not the files on disk: git may have them
// otherwise (core.autocrlf, a file it ignores), and a signature of the files
// on disk would not verify for anyone who fetches the tag.
func tagVersion(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string, stack bool) error {
	top, rel, err := repositoryOf(ctx, dir)
	if err != nil {
		return err
	}

	// Before the commit is read: a module that was never committed is told
	// so, not that the commit has no such module.
	if err := checkCommitted(ctx, top, rel); err != nil {
		return err
	}

	var key *ecdsa.PrivateKey

	if keyFile != "" {
		if key, err = readPrivateKey(keyFile); err != nil {
			return err
		}
	}

	h, err := readHead(ctx, logger, stderr, dir, top, rel)
	if err != nil {
		return err
	}

	if stack && h.what != whatStack {
		return errUnpublished
	}

	tag, err := publishable(ctx, top, rel, h)
	if err != nil {
		return err
	}

	commit := h.commit

	if key == nil {
		warn(stderr, "%s %s is not signed (-key): the global registry lists only signed versions", h.name, h.version)
	} else if commit, err = commitSignature(ctx, stdout, top, rel, dir, key, h); err != nil {
		return err
	}

	if _, err := gitOut(ctx, top, "tag", "-a", "-m", h.name+" "+h.version, tag, commit); err != nil {
		return err
	}

	_, err = fmt.Fprintf(stdout, "tagged %s %s %s as %s at %s; push it: git push origin %s\n", h.what, h.name, h.version, tag, commit, tag)

	return err
}

// head is the module or published stack of a directory as the commit of
// HEAD has it.
type head struct {
	signed

	commit string
	// dirs are the directories of the repository the tree digest covers:
	// of a module and its child modules, or of a stack.
	dirs []string
}

// readHead reads the module or stack in the directory rel of the repository
// at top from the commit of HEAD, and validates it. The tree of the commit
// is extracted by the fetcher, as it is for a registry and for everyone who
// installs the version: the files as they are committed, the regular ones
// only. dir is the directory as the user named it, for the diagnostics.
func readHead(ctx context.Context, logger *slog.Logger, stderr io.Writer, dir, top, rel string) (head, error) {
	commit, err := gitOut(ctx, top, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return head{}, err
	}

	// The directory of the objects, which a linked work tree shares with
	// its repository: the fetcher reads a repository there.
	gitDir, err := gitOut(ctx, top, "rev-parse", "--git-common-dir")
	if err != nil {
		return head{}, err
	}

	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(top, gitDir)
	}

	tmp, err := os.MkdirTemp("", "rigger-publish-")
	if err != nil {
		return head{}, err
	}

	defer os.RemoveAll(tmp) //nolint:errcheck // a temporary directory

	tree, err := fetch.New(tmp).Checkout(ctx, gitDir, commit)
	if err != nil {
		return head{}, err
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return head{}, err
	}

	t, dirs, diags, err := readCommitted(ctx, eng, os.DirFS(tree), rel)
	if err != nil {
		return head{}, err
	}

	if diags.HasErrors() {
		if err := writeDiagsOf(stderr, fmt.Sprintf("%s as the commit %s has it", dir, commit[:12]), tree, rel, diags); err != nil {
			return head{}, err
		}

		return head{}, errCommitted
	}

	return head{signed: t, commit: commit, dirs: dirs}, nil
}

// readCommitted loads and validates the module or the published stack (a
// directory with stack.hcl) in the directory rel of root, the tree of a
// commit, as a registry does before it lists a version. It returns what a
// signature names of it and the directories its digest covers.
func readCommitted(ctx context.Context, eng *rigger.Engine, root fs.FS, rel string) (signed, []string, hcl.Diagnostics, error) {
	if _, err := fs.Stat(root, path.Join(rel, stackFile)); err == nil {
		return readCommittedStack(ctx, eng, root, rel)
	}

	m, diags := eng.LoadModuleAt(ctx, root, rel)
	if !diags.HasErrors() {
		diags = append(diags, eng.ValidateModule(ctx, m)...)
	}

	if diags.HasErrors() {
		return signed{}, nil, diags, nil
	}

	digest, err := m.Digest()
	md := m.Metadata()
	_, dirs := m.Files()

	return signed{what: whatModule, name: md.Name, version: md.Version, digest: digest}, dirs, nil, err
}

// readCommittedStack is readCommitted of a stack: validated with each of
// its value files, as a registry validates it.
func readCommittedStack(ctx context.Context, eng *rigger.Engine, root fs.FS, rel string) (signed, []string, hcl.Diagnostics, error) {
	s, diags := eng.LoadStack(ctx, root, rel)
	if diags.HasErrors() {
		return signed{}, nil, diags, nil
	}

	md := s.Metadata()
	if md == nil {
		return signed{}, nil, nil, errUnpublished
	}

	diags, err := validateValueFiles(ctx, eng, s)
	if err != nil || diags.HasErrors() {
		return signed{}, nil, diags, err
	}

	digest, err := s.Digest()

	return signed{what: whatStack, name: md.Name, version: md.Version, digest: digest}, []string{rel}, nil, err
}

// publishable checks that the version of h can be tagged, and returns its
// tag: it has a version, which is not tagged yet, and no directory its
// digest covers has changes that are not committed.
func publishable(ctx context.Context, top, rel string, h head) (string, error) {
	if h.version == "" {
		return "", errNoVersion
	}

	tag := "v" + h.version
	if rel != "." {
		tag = path.Base(rel) + "/v" + h.version
	}

	if _, err := gitOut(ctx, top, "rev-parse", "--verify", "--quiet", "refs/tags/"+tag); err == nil {
		return "", fmt.Errorf("%s: %w", tag, errTagExists)
	}

	return tag, checkCommitted(ctx, top, h.dirs...)
}

// checkCommitted checks that directories of the repository at top have no
// changes that are not committed: what is published is the commit, and a
// change left on disk would not be in it.
func checkCommitted(ctx context.Context, top string, dirs ...string) error {
	status, err := gitOut(ctx, top, append([]string{"status", "--porcelain", "--"}, dirs...)...)
	if err != nil {
		return err
	}

	if status != "" {
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

// commitSignature signs the version of h with key, writes the signature
// into the .rigger-signatures of dir and commits it, if it is new. It returns
// the commit to tag: that of the signature, which has the files of the
// signed commit and nothing else of the version.
func commitSignature(ctx context.Context, stdout io.Writer, top, rel, dir string, key *ecdsa.PrivateKey, h head) (string, error) {
	file, id, err := signatureFile(dir, key)
	if err != nil {
		return "", err
	}

	sigDir := path.Join(rel, sign.SignatureDir)

	committed, err := writeAndCommit(ctx, top, sigDir, file, key, h)
	if err != nil {
		return "", err
	}

	if err := reportSigned(stdout, h.signed, id, file); err != nil {
		return "", err
	}

	if !committed {
		return h.commit, nil
	}

	commit, err := gitOut(ctx, top, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}

	// The digest was signed for h.commit. A commit that changed more than
	// the signatures (a hook that adds files, a commit made meanwhile) has
	// another digest, and its tag would not verify.
	diff := append([]string{"diff", "--quiet", h.commit, commit, "--"}, h.dirs...)

	_, err = gitOut(ctx, top, append(diff, ":(exclude)"+sigDir)...)
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", fmt.Errorf("%w: between the signed commit %s and %s", errSignedTree, h.commit, commit)
	}

	return commit, err
}

// writeAndCommit writes the signature of h by key to file, in the directory
// sigDir of the repository, and commits it; it reports whether there was
// anything to commit.
//
// When the signature is not committed (a hook refuses the commit, git has
// no author, gpg fails) it puts the file and the index back as they were. A
// signature left staged or on disk is a change of the module, and every
// later publish would refuse it (errDirty).
func writeAndCommit(ctx context.Context, top, sigDir, file string, key *ecdsa.PrivateKey, h head) (committed bool, err error) {
	old, readErr := os.ReadFile(file) //nolint:gosec // G304: the signature file of the module
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return false, readErr
	}

	defer func() {
		if err == nil {
			return
		}

		// Also when the command was cancelled: that is how a commit may fail.
		_, _ = gitOut(context.WithoutCancel(ctx), top, "reset", "--quiet", "--", sigDir)

		if readErr == nil {
			_ = os.WriteFile(file, old, 0o644) //nolint:gosec // G306: a signature is public

			return
		}

		// The directory too, if the signature was its only file.
		_ = os.Remove(file)
		_ = os.Remove(filepath.Dir(file))
	}()

	if err := writeSignature(file, key, h.signed); err != nil {
		return false, err
	}

	if _, err := gitOut(ctx, top, "add", "--", sigDir); err != nil {
		return false, err
	}

	staged, err := gitOut(ctx, top, "diff", "--cached", "--name-only", "--", sigDir)
	if err != nil || staged == "" {
		return false, err
	}

	_, err = gitOut(ctx, top, "commit", "--quiet", "-m", h.name+": sign "+h.version, "--", sigDir)

	return err == nil, err
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
