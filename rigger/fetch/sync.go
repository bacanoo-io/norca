// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"context"
	"errors"
	"fmt"
	"os"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A host that keeps registries for a long time (a server,
// design/rig-integration.md 4.5) syncs a registry when someone asks for it,
// and reads it at the commit of that sync until the next one: Sync fetches,
// Checkout and TagsAt read the mirror as it is and fetch only when it lacks
// the commit (a mirror made on another server, or before a restart).

// ErrNoCommit is the error of a commit that the repository does not have,
// also after a fetch.
var ErrNoCommit = errors.New("the repository has no such commit")

// Sync fetches the mirror of a repository now, also when it was fetched
// earlier in this run, and returns the commit of its default branch. A
// repository on the local file system is read in place.
func (f *Fetcher) Sync(ctx context.Context, repository string) (string, error) {
	r, err := f.refresh(ctx, repository)
	if err != nil {
		return "", err
	}

	commit, err := headCommit(r)
	if err != nil {
		return "", fmt.Errorf("%s: the default branch: %w", repository, err)
	}

	return commit, nil
}

// Checkout returns the directory of the tree of a commit of a repository in
// the cache, extracted from the mirror (fetched only when the mirror lacks
// the commit). The directory is absolute and is not changed afterwards.
func (f *Fetcher) Checkout(ctx context.Context, repository, commit string) (string, error) {
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("%s: invalid commit %q", repository, commit)
	}

	r, err := f.repoWith(ctx, repository, commit)
	if err != nil {
		return "", err
	}

	tree := f.TreeDir(commit)
	if _, err := os.Stat(tree); err != nil {
		if err := extract(r, commit, tree); err != nil {
			return "", err
		}
	}

	return tree, nil
}

// TagsAt lists the tags that start with prefix and name the commit or one
// before it in its history: the versions a registry repository had when
// it was synced at that commit. A tag made later on a later commit is not
// among them. The mirror is fetched only when it lacks the commit.
func (f *Fetcher) TagsAt(ctx context.Context, repository, commit, prefix string) ([]Tag, error) {
	r, err := f.repoWith(ctx, repository, commit)
	if err != nil {
		return nil, err
	}

	history, err := ancestors(r, commit)
	if err != nil {
		return nil, err
	}

	tags, err := tagCommits(r, prefix)
	if err != nil {
		return nil, err
	}

	out := tags[:0]

	for _, tag := range tags {
		if history[tag.Commit] {
			out = append(out, tag)
		}
	}

	return out, nil
}

// refresh opens a repository: in place on the local file system, else its
// mirror, fetched now.
func (f *Fetcher) refresh(ctx context.Context, repository string) (*git.Repository, error) {
	if _, ok := localPath(repository); ok {
		return f.openRepo(ctx, repository)
	}

	url, dir := repositoryURL(repository), f.mirrorDir(repository)

	unlock, err := f.repos.lock(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	auth, err := f.auth(ctx, repository, url)
	if err != nil {
		return nil, err
	}

	r, err := f.updateMirror(ctx, url, dir, auth)
	if err != nil {
		return nil, err
	}

	f.setUpdated(dir)

	return r, nil
}

// repoWith opens a repository that has the commit: in place on the local
// file system, else its mirror as it is, fetched once when it is missing or
// lacks the commit.
func (f *Fetcher) repoWith(ctx context.Context, repository, commit string) (*git.Repository, error) {
	var r *git.Repository

	if dir, ok := localPath(repository); ok {
		local, err := git.PlainOpen(dir)
		if err != nil {
			return nil, fmt.Errorf("git repository %s: %w", dir, err)
		}

		r = local
	} else if mirror, err := git.PlainOpen(f.mirrorDir(repository)); err == nil {
		r = mirror
	}

	if r == nil || !hasCommit(r, commit) {
		if _, ok := localPath(repository); ok {
			return nil, fmt.Errorf("%s %s: %w", repository, commit, ErrNoCommit)
		}

		fetched, err := f.refresh(ctx, repository)
		if err != nil {
			return nil, err
		}

		if !hasCommit(fetched, commit) {
			return nil, fmt.Errorf("%s %s: %w", repository, commit, ErrNoCommit)
		}

		r = fetched
	}

	return r, nil
}

func hasCommit(r *git.Repository, commit string) bool {
	_, err := r.CommitObject(plumbing.NewHash(commit))

	return err == nil
}

// ancestors returns the commit and every commit before it.
func ancestors(r *git.Repository, commit string) (map[string]bool, error) {
	iter, err := r.Log(&git.LogOptions{From: plumbing.NewHash(commit)})
	if err != nil {
		return nil, err
	}

	out := map[string]bool{}

	err = iter.ForEach(func(c *object.Commit) error {
		out[c.Hash.String()] = true

		return nil
	})

	return out, err
}
