// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fetch

import (
	"errors"
	"fmt"
	"io"
)

// Limits bound what the fetcher writes into the cache of one commit of a
// git repository (design 12.4: limits the host sets). A tree is small in a
// repository and may be large on the disk: a blob of zeros of many
// gigabytes packs into kilobytes, and a tree may name millions of files.
type Limits struct {
	// TreeBytes bounds the files of the tree of one commit together.
	TreeBytes int64
	// FileBytes bounds one file of a tree.
	FileBytes int64
	// TreeFiles bounds the number of files of a tree.
	TreeFiles int
}

// DefaultLimits returns the limits of a fetcher unless its host sets
// others. They are wide: they stop a tree that is made to fill a disk, not
// a repository that is merely large. A host that fetches what others name
// (a server) sets its own.
func DefaultLimits() Limits {
	return Limits{TreeBytes: 4 << 30, FileBytes: 1 << 30, TreeFiles: 1_000_000}
}

// SetLimits sets the limits of the fetcher and of those made of it
// (WithGlobalRegistry); a limit that is not positive keeps its default. It
// is set before the fetcher is used.
func (f *Fetcher) SetLimits(l Limits) {
	d := DefaultLimits()

	if l.TreeBytes <= 0 {
		l.TreeBytes = d.TreeBytes
	}

	if l.FileBytes <= 0 {
		l.FileBytes = d.FileBytes
	}

	if l.TreeFiles <= 0 {
		l.TreeFiles = d.TreeFiles
	}

	f.limits = l
}

// ErrTreeTooLarge is the error of a commit whose tree is past a limit of
// the fetcher (Limits).
var ErrTreeTooLarge = errors.New("the tree of the commit is past a limit of the fetcher")

// treeBudget counts what is written of one tree against its limits.
type treeBudget struct {
	limits Limits
	bytes  int64
	files  int
}

// file takes a file of the size its blob declares into the count, before
// any of it is read: the error of a tree past a limit.
func (b *treeBudget) file(name string, size int64) error {
	b.files++

	switch {
	case b.files > b.limits.TreeFiles:
		return fmt.Errorf("%w: more than %d files", ErrTreeTooLarge, b.limits.TreeFiles)
	case size > b.limits.FileBytes:
		return fmt.Errorf("%w: %s has %d bytes, more than %d", ErrTreeTooLarge, name, size, b.limits.FileBytes)
	case size > b.limits.TreeBytes-b.bytes:
		return fmt.Errorf("%w: more than %d bytes", ErrTreeTooLarge, b.limits.TreeBytes)
	default:
		return nil
	}
}

// reader returns r as the content of a file: it fails when it has more
// bytes than a file or the rest of the tree may have. What a blob declares
// as its size is not what its stream must unpack to.
func (b *treeBudget) reader(name string, r io.Reader) io.Reader {
	return &budgetReader{budget: b, name: name, r: r, left: min(b.limits.FileBytes, b.limits.TreeBytes-b.bytes)}
}

type budgetReader struct {
	budget *treeBudget
	name   string
	r      io.Reader
	left   int64
}

func (r *budgetReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)

	r.left -= int64(n)
	r.budget.bytes += int64(n)

	if r.left < 0 {
		return n, fmt.Errorf("%w: %s unpacks to more than its bound (a file %d bytes, a tree %d)",
			ErrTreeTooLarge, r.name, r.budget.limits.FileBytes, r.budget.limits.TreeBytes)
	}

	return n, err
}
