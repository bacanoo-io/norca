// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A tag is named, not a path to another reference: a branch moves.
func TestTagCommit_OnlyTags(t *testing.T) {
	t.Parallel()

	r, err := git.PlainInit(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := wt.Commit("first", &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	head, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}

	name := "../heads/" + head.Name().Short()

	if got, err := tagCommit(r, name); err == nil {
		t.Errorf("tagCommit(%q) = %s, a branch was taken as a tag", name, got)
	}
}
