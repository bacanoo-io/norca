// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// The fetcher reads git repositories with go-git, in the process: a server
// that embeds Rigger needs no git program (design/rig-integration.md 4.2 of
// norca). A repository on a URL is kept as a bare mirror in the cache; a
// repository on the local file system is read where it is.

// Credentials are what the fetcher authenticates to a git repository with.
// The host gives them per repository (SetCredentials); nil is anonymous.
type Credentials struct {
	// Username and Password authenticate over http(s); a token is the
	// password (the username is then "git" unless given).
	Username string
	Password string

	// SSHUser is the user of an ssh URL ("git" unless given). SSHKey is a
	// private key in PEM, with SSHKeyPassphrase if it is encrypted; without
	// a key, SSHAgent authenticates with the keys of the ssh agent.
	SSHUser          string
	SSHKey           []byte
	SSHKeyPassphrase string
	SSHAgent         bool

	// KnownHosts are the known hosts of ssh, in the format of the
	// known_hosts file; empty uses the files of the user (SSH_KNOWN_HOSTS,
	// else ~/.ssh/known_hosts).
	KnownHosts []byte
}

// CredentialsFunc returns the credentials of a repository (its address as
// a module source names it), nil for none.
type CredentialsFunc func(ctx context.Context, repository string) (*Credentials, error)

// SetCredentials sets how the fetcher finds the credentials of a
// repository. Without it every repository is read anonymously.
func (f *Fetcher) SetCredentials(fn CredentialsFunc) {
	f.credentials = fn
}

// localPath returns the directory of a repository on the local file
// system: a file:// URL or an absolute path.
func localPath(repo string) (string, bool) {
	if p, ok := strings.CutPrefix(repo, "file://"); ok {
		return filepath.FromSlash(p), true
	}

	if strings.HasPrefix(repo, "/") || filepath.IsAbs(repo) {
		return repo, true
	}

	return "", false
}

// auth returns the authentication of a repository, nil for anonymous.
func (f *Fetcher) auth(ctx context.Context, repo, url string) (transport.AuthMethod, error) {
	if f.credentials == nil {
		return nil, nil
	}

	c, err := f.credentials(ctx, repo)
	if err != nil || c == nil {
		return nil, err
	}

	if !strings.HasPrefix(url, "ssh://") {
		if c.Password == "" {
			return nil, nil
		}

		return &githttp.BasicAuth{Username: cmp.Or(c.Username, "git"), Password: c.Password}, nil
	}

	return sshAuth(repo, c)
}

// sshAuth returns the ssh authentication of credentials: a key, else the
// agent, with the known hosts given; nil without either.
func sshAuth(repo string, c *Credentials) (transport.AuthMethod, error) {
	user := cmp.Or(c.SSHUser, "git")

	var cb gossh.HostKeyCallback

	if len(c.KnownHosts) > 0 {
		var err error
		if cb, err = knownHostsCallback(c.KnownHosts); err != nil {
			return nil, fmt.Errorf("the known hosts of %s: %w", repo, err)
		}
	}

	switch {
	case len(c.SSHKey) > 0:
		keys, err := gitssh.NewPublicKeys(user, c.SSHKey, c.SSHKeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("the ssh key of %s: %w", repo, err)
		}

		if cb != nil {
			keys.HostKeyCallback = cb
		}

		return keys, nil
	case c.SSHAgent:
		agent, err := gitssh.NewSSHAgentAuth(user)
		if err != nil {
			return nil, fmt.Errorf("the ssh agent for %s: %w", repo, err)
		}

		if cb != nil {
			agent.HostKeyCallback = cb
		}

		return agent, nil
	default:
		return nil, nil
	}
}

// knownHostsCallback returns the host key check of known hosts given as
// bytes: the parser of known_hosts reads files only, so they pass through a
// file of their own that is removed at once (the callback keeps what it
// read).
func knownHostsCallback(data []byte) (gossh.HostKeyCallback, error) {
	tmp, err := os.CreateTemp("", "rigger-known-hosts-")
	if err != nil {
		return nil, err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck // a temporary file

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return nil, err
	}

	if err := tmp.Close(); err != nil {
		return nil, err
	}

	return gitssh.NewKnownHostsCallback(tmp.Name())
}

// openRepo returns the repository of repo: read in place when it is on the
// local file system, else its mirror in the cache, made or fetched once per
// run.
func (f *Fetcher) openRepo(ctx context.Context, repo string) (*git.Repository, error) {
	if dir, ok := localPath(repo); ok {
		r, err := git.PlainOpen(dir)
		if err != nil {
			return nil, fmt.Errorf("git repository %s: %w", dir, err)
		}

		return r, nil
	}

	url, dir := repositoryURL(repo), f.mirrorDir(repo)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updated[dir] {
		return git.PlainOpen(dir)
	}

	auth, err := f.auth(ctx, repo, url)
	if err != nil {
		return nil, err
	}

	r, err := f.updateMirror(ctx, url, dir, auth)
	if err != nil {
		return nil, err
	}

	f.updated[dir] = true

	return r, nil
}

// updateMirror fetches the mirror in dir, or clones it when there is none.
func (f *Fetcher) updateMirror(ctx context.Context, url, dir string, auth transport.AuthMethod) (*git.Repository, error) {
	if r, err := git.PlainOpen(dir); err == nil {
		err := r.FetchContext(ctx, &git.FetchOptions{
			RemoteName: "origin",
			RefSpecs:   []config.RefSpec{"+refs/*:refs/*"},
			Auth:       auth,
			Prune:      true,
			Force:      true,
			Tags:       git.AllTags,
		})
		if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
			return nil, fmt.Errorf("git fetch %s: %w", url, err)
		}

		return r, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, err
	}

	r, err := git.PlainCloneContext(ctx, dir, true, &git.CloneOptions{URL: url, Auth: auth, Mirror: true, Tags: git.AllTags})
	if err != nil {
		_ = os.RemoveAll(dir)

		return nil, fmt.Errorf("git clone %s: %w", url, err)
	}

	return r, nil
}

// peel returns the commit a tag or a commit hash names.
func peel(r *git.Repository, h plumbing.Hash) (plumbing.Hash, error) {
	if tag, err := r.TagObject(h); err == nil {
		c, err := tag.Commit()
		if err != nil {
			return plumbing.ZeroHash, err
		}

		return c.Hash, nil
	}

	c, err := r.CommitObject(h)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return c.Hash, nil
}

// tagCommits returns the tags of the repository that start with prefix and
// the commits they name.
func tagCommits(r *git.Repository, prefix string) ([]Tag, error) {
	iter, err := r.Tags()
	if err != nil {
		return nil, err
	}

	var out []Tag

	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if !strings.HasPrefix(name, prefix) {
			return nil
		}

		commit, err := peel(r, ref.Hash())
		if err != nil {
			return nil //nolint:nilerr // a tag of something that is not a commit is not a version
		}

		out = append(out, Tag{Name: name, Commit: commit.String()})

		return nil
	})

	return out, err
}

// tagCommit returns the commit of the tag name.
func tagCommit(r *git.Repository, name string) (string, error) {
	ref, err := r.Tag(name)
	if err != nil {
		return "", err
	}

	commit, err := peel(r, ref.Hash())
	if err != nil {
		return "", err
	}

	return commit.String(), nil
}

// headCommit returns the commit of HEAD.
func headCommit(r *git.Repository) (string, error) {
	ref, err := r.Head()
	if err != nil {
		return "", err
	}

	commit, err := peel(r, ref.Hash())
	if err != nil {
		return "", err
	}

	return commit.String(), nil
}

// writeTree writes the regular files of the tree of a commit into dir,
// through an os.Root (as an archive of the commit would be extracted:
// links and submodules are not part of a module).
func writeTree(r *git.Repository, commit, dir string) error {
	c, err := r.CommitObject(plumbing.NewHash(commit))
	if err != nil {
		return err
	}

	tree, err := c.Tree()
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the files are closed one by one

	return tree.Files().ForEach(func(file *object.File) error {
		if file.Mode != filemode.Regular && file.Mode != filemode.Executable {
			return nil
		}

		rd, err := file.Reader()
		if err != nil {
			return err
		}

		err = writeEntry(root, path.Clean(file.Name), io.Reader(rd))
		closeErr := rd.Close()

		return cmp.Or(err, closeErr)
	})
}
