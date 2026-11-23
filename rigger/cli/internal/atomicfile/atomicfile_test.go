// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package atomicfile

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

const (
	oldText  = "old\n"
	newText  = "new\n"
	fileName = "file"
)

// modes reports whether files have the modes of unix here, and whether a
// mode stops the user of the test (it stops no root).
func modes() (kept, enforced bool) {
	kept = runtime.GOOS != "windows"

	return kept, kept && os.Geteuid() != 0
}

// only checks that the directory has the files named and nothing else: no
// file of a write is left beside them.
func only(t *testing.T, dir string, names ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}

	if !slices.Equal(got, names) {
		t.Fatalf("the directory has %v, want %v alone", got, names)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()

	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

// fileWith makes a file with the old text and a mode in a new directory.
func fileWith(t *testing.T, mode fs.FileMode) (dir, p string) {
	t.Helper()

	dir = t.TempDir()
	p = filepath.Join(dir, fileName)

	if err := os.WriteFile(p, []byte(oldText), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}

	return dir, p
}

// A new file has the data, and the mode asked for.
func TestWrite_NewFile(t *testing.T) {
	t.Parallel()

	keptModes, _ := modes()
	dir := t.TempDir()
	p := filepath.Join(dir, fileName)

	if err := Write(p, []byte(newText), 0o640); err != nil {
		t.Fatal(err)
	}

	if got := read(t, p); got != newText {
		t.Errorf("the file = %q, want %q", got, newText)
	}

	// The mode is perm without what the umask of the test takes.
	if info, err := os.Stat(p); err != nil || (keptModes && info.Mode().Perm()&^0o640 != 0) {
		t.Errorf("the mode = %v, %v, want no more than 640", info.Mode().Perm(), err)
	}

	only(t, dir, fileName)
}

// A file that is there is replaced by another file, which has its mode.
func TestWrite_ReplacesTheFile(t *testing.T) {
	t.Parallel()

	keptModes, _ := modes()
	dir, p := fileWith(t, 0o660)

	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	// On Windows a FileInfo reads what tells files apart from the path when
	// it is first compared: now, while the path still names this file.
	os.SameFile(before, before)

	if err := Write(p, []byte(newText), 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	if os.SameFile(before, after) {
		t.Error("the file was written in place")
	}

	if keptModes && after.Mode().Perm() != 0o660 {
		t.Errorf("the mode = %o, want the 660 the file had", after.Mode().Perm())
	}

	if got := read(t, p); got != newText {
		t.Errorf("the file = %q, want %q", got, newText)
	}

	only(t, dir, fileName)
}

// A link stays a link: the file it names is the one replaced.
func TestWrite_ThroughALink(t *testing.T) {
	t.Parallel()

	dir, target := fileWith(t, 0o600)
	link := filepath.Join(dir, "link")

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no links here: %v", err)
	}

	if err := Write(link, []byte(newText), 0o600); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link is %v, %v: it is no link any more", info.Mode(), err)
	}

	if got := read(t, target); got != newText {
		t.Errorf("the file of the link = %q, want %q", got, newText)
	}

	only(t, dir, fileName, "link")
}

// A write that fails leaves the file as it was, and no file of its own.
func TestWrite_Fails(t *testing.T) {
	t.Parallel()

	_, enforcedModes := modes()

	t.Run("a file that may not be written", func(t *testing.T) {
		t.Parallel()

		if !enforcedModes {
			t.Skip("a mode stops no write here")
		}

		dir, p := fileWith(t, 0o400)

		if err := Write(p, []byte(newText), 0o600); err == nil {
			t.Error("Write() of a read-only file = nil, want an error")
		}

		if got := read(t, p); got != oldText {
			t.Errorf("the file = %q, want it as it was", got)
		}

		only(t, dir, fileName)
	})

	t.Run("a directory in its place", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		p := filepath.Join(dir, fileName)

		if err := os.Mkdir(p, 0o750); err != nil {
			t.Fatal(err)
		}

		if err := Write(p, []byte(newText), 0o600); err == nil {
			t.Error("Write() over a directory = nil, want an error")
		}

		only(t, dir, fileName)
	})

	t.Run("no directory", func(t *testing.T) {
		t.Parallel()

		if err := Write(filepath.Join(t.TempDir(), "none", fileName), []byte(newText), 0o600); err == nil {
			t.Error("Write() into no directory = nil, want an error")
		}
	})
}

// Who has the file open while it is written reads all of the old content:
// the write does not touch that file.
func TestWrite_ReaderOfTheOldFile(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("an open file can't be replaced on Windows")
	}

	_, p := fileWith(t, 0o600)

	reader, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}

	defer reader.Close() //nolint:errcheck // read only

	if err := Write(p, []byte(newText), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := io.ReadAll(reader); err != nil || string(got) != oldText {
		t.Errorf("the reader of the old file has %q, %v, want %q", got, err, oldText)
	}

	if got := read(t, p); got != newText {
		t.Errorf("the file = %q, want %q", got, newText)
	}
}

// The file of a secret has its mode whatever mode the file before it had,
// also one that may not be written.
func TestWriteMode(t *testing.T) {
	t.Parallel()

	keptModes, _ := modes()

	for _, mode := range []fs.FileMode{0o644, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()

			if !keptModes && mode&0o200 == 0 {
				t.Skip("Windows replaces no file that is read-only")
			}

			dir, p := fileWith(t, mode)

			if err := WriteMode(p, []byte(newText), 0o600); err != nil {
				t.Fatal(err)
			}

			if info, err := os.Stat(p); err != nil || (keptModes && info.Mode().Perm() != 0o600) {
				t.Errorf("the mode = %v, %v, want 600", info.Mode().Perm(), err)
			}

			if got := read(t, p); got != newText {
				t.Errorf("the file = %q, want %q", got, newText)
			}

			only(t, dir, fileName)
		})
	}
}
