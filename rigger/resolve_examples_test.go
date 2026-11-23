// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	rigger "github.com/bacanoo-io/norca/rigger"
	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/sign"
	"github.com/bacanoo-io/norca/rigger/source"
)

// The lock files of the stacks of examples/ are the files a sync writes,
// and the tests keep them so (TestSyncStack_Examples). After a change of a
// module of examples/registry, or of the modules a stack of examples/ names,
// they are written again with
//
//	go test -count=1 -run '^TestSyncStack_Examples$' . -update
//
// in the directory of the module (make examples). That signs the modules of
// the registry again when a signature no longer holds, with a new key whose
// public half goes into the stacks and whose private half is thrown away,
// and then writes the lock file of every stack.
var updateExamples = flag.Bool("update", false, "write the signatures and the lock files of examples/ again")

const (
	// examplesDir is examples/ of the module, seen from this package.
	examplesDir = "examples"
	// exampleRepository is the repository of the registry "example" of the
	// stacks of examples/. No such repository is on the network:
	// examples/registry is what it holds.
	exampleRepository = "github.com/example/rigger-registry"
	// exampleKeyFile is the name of the file of a stack with the public key
	// the modules of the example registry are signed with.
	exampleKeyFile = "example-registry.pub"
	// updateHint is how the files of examples/ are written again.
	updateHint = "write it again: go test -count=1 -run '^TestSyncStack_Examples$' . -update (in rigger/)"
)

// notSynced are the stacks of examples/ no sync is run for, each with the
// reason. Their lock files are checked for their form alone: they parse,
// they are what Lock.Bytes writes, and they hold the sources of their stack
// (TestParseLock_Examples and TestLoadStack_Examples of internal/lang).
var notSynced = map[string]string{
	"registry/stacks/observability": "its modules are of another registry (github.com/bacanoo-io/norca-registry), " +
		"which examples/ does not hold: there is no version to resolve, no content to take a digest of and no " +
		"signature of the key of that registry, so the entries of its lock file are written by hand",
}

// exampleStacks returns the directories of the stacks of examples/, found
// by walking: a stack that is added is tested without a change of the tests.
func exampleStacks(t *testing.T) []string {
	t.Helper()

	dirs := exampleFiles(t, ".", func(p string) bool { return path.Base(p) == "stack.hcl" })
	if len(dirs) == 0 {
		t.Fatal("examples/ has no stack")
	}

	for i, p := range dirs {
		dirs[i] = path.Dir(p)
	}

	return dirs
}

// exampleFiles returns the files below dir of examples/ that match, by
// their paths in examples/.
func exampleFiles(t *testing.T, dir string, match func(p string) bool) []string {
	t.Helper()

	var files []string

	err := fs.WalkDir(os.DirFS(examplesDir), dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && match(p) {
			files = append(files, p)
		}

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return files
}

// registryModule is a module of examples/registry that is published: a
// directory of modules/, tagged <name>/v<version> (design 7.2).
type registryModule struct {
	// dir is the directory in examples/.
	dir                   string
	name, version, digest string
}

// registryModules loads the published modules of examples/registry.
func registryModules(t *testing.T, e *rigger.Engine) []registryModule {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(examplesDir, "registry", "modules"))
	if err != nil {
		t.Fatal(err)
	}

	var mods []registryModule

	for _, entry := range entries {
		dir := path.Join("registry/modules", entry.Name())
		if _, err := os.Stat(filepath.Join(examplesDir, filepath.FromSlash(dir), metadataFile)); err != nil {
			continue // a directory of child modules (lib/): they have no tag of their own
		}

		m, diags := e.LoadModuleAt(t.Context(), os.DirFS(examplesDir), dir)
		if diags.HasErrors() {
			t.Fatalf("%s: %s", dir, diags)
		}

		digest, err := m.Digest()
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}

		md := m.Metadata()
		mods = append(mods, registryModule{dir: dir, name: md.Name, version: md.Version, digest: digest})
	}

	if len(mods) == 0 {
		t.Fatal("examples/registry has no module")
	}

	return mods
}

// staleSignatures says what of the signatures of the example registry does
// not hold: the stacks that trust the registry carry one public key, in a
// file named exampleKeyFile, and every published module has one signature,
// which is of that key and of the digest the module has now. Nothing is
// checked when no stack carries the key.
func staleSignatures(t *testing.T, mods []registryModule, keyFiles []string) []string {
	t.Helper()

	if len(keyFiles) == 0 {
		return nil
	}

	var (
		stale []string
		key   sign.PublicKey
	)

	for i, p := range keyFiles {
		text, err := os.ReadFile(filepath.Join(examplesDir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}

		k, err := sign.ParsePublicKey(text)

		switch {
		case err != nil:
			return []string{fmt.Sprintf("%s: %v", p, err)}
		case i == 0:
			key = k
		case k.ID != key.ID:
			stale = append(stale, fmt.Sprintf("%s is another key than %s", p, keyFiles[0]))
		}
	}

	for _, m := range mods {
		sigs, err := sign.ReadSignatures(os.DirFS(examplesDir), m.dir)
		if err != nil {
			t.Fatal(err)
		}

		if len(sigs) != 1 || len(sign.Verify(m.digest, sigs, []sign.PublicKey{key})) != 1 {
			stale = append(stale, fmt.Sprintf("%s (%s %s, now %s): its signatures (%d) are not one signature of that digest by the key %s",
				m.dir, m.name, m.version, m.digest, len(sigs), key.ID))
		}
	}

	return stale
}

// signExampleRegistry signs every published module of examples/registry
// with a new key, as rigger module publish does (.rigger-signatures of the
// module), and writes the public key into the stacks that carry it. The
// private key is not kept: the next change of a module makes a new one.
func signExampleRegistry(t *testing.T, mods []registryModule, keyFiles []string) {
	t.Helper()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	pub, err := sign.MarshalPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	id, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	header := "The public key the modules of the example registry (examples/registry) are\n" +
		"signed with. The tests write it, with the signatures and the lock files\n" +
		"(examples/README.md); its private key was thrown away.\n\n"

	for _, p := range keyFiles {
		writeExample(t, p, append([]byte(header), pub...))
	}

	for _, m := range mods {
		sigDir := filepath.Join(examplesDir, filepath.FromSlash(m.dir), sign.SignatureDir)
		if err := os.RemoveAll(sigDir); err != nil {
			t.Fatal(err)
		}

		s, err := sign.Sign(key, m.digest, m.name, m.version)
		if err != nil {
			t.Fatal(err)
		}

		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			t.Fatal(err)
		}

		writeExample(t, path.Join(m.dir, sign.SignatureDir, sign.FileName(id)), append(data, '\n'))
	}
}

// writeExample writes a file of examples/.
func writeExample(t *testing.T, p string, data []byte) {
	t.Helper()

	file := filepath.Join(examplesDir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, data, 0o644); err != nil { //nolint:gosec // G306: a file of the repository, public
		t.Fatal(err)
	}

	t.Logf("wrote %s", file)
}

// exampleRegistry serves the example registry to an engine: the requests
// for exampleRepository go to a git repository on the disk, through the
// fetcher of Rigger, and the built-in modules to the fetcher as they are.
// Nothing else is fetched: the tests of the examples use no network.
type exampleRegistry struct {
	// dir is the repository on the disk.
	dir   string
	inner *fetch.Fetcher
}

func (r exampleRegistry) local(req source.Request) (source.Request, error) {
	switch {
	case req.Kind == source.KindBuiltin:
		return req, nil
	case req.Kind == source.KindGit && req.Repository == exampleRepository:
		req.Repository = r.dir

		return req, nil
	default:
		return req, fmt.Errorf("%s (%s %q) is not of %s, the one registry examples/ holds; the tests fetch nothing over a network: %w",
			req.Source, req.Kind, req.Repository, exampleRepository, fs.ErrNotExist)
	}
}

func (r exampleRegistry) Versions(ctx context.Context, req source.Request) ([]source.Available, error) {
	req, err := r.local(req)
	if err != nil {
		return nil, err
	}

	return r.inner.Versions(ctx, req)
}

func (r exampleRegistry) Fetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	req, err := r.local(req)
	if err != nil {
		return source.Fetched{}, err
	}

	return r.inner.Fetch(ctx, req)
}

// newExampleRegistry makes the git repository of the example registry as
// it is when its modules are published: one commit with modules/ of
// examples/registry, the signatures included, and the tag <name>/v<version>
// of every module on it.
//
// The commit is the same on every machine, because a lock file records it:
// the objects are written from the files as they are, not through a working
// tree of git (every file is a regular file, whatever the system says of
// its mode), the author and the time are fixed, and examples/registry keeps
// LF on every checkout by its .gitattributes.
func newExampleRegistry(t *testing.T, mods []registryModule) exampleRegistry {
	t.Helper()

	dir := t.TempDir()

	r, err := git.PlainInit(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	// What a module is: its files, and of the hidden ones its signatures.
	// Another hidden file (one a system or an editor leaves) is no part of
	// the registry, and would make another commit.
	published := func(p string) bool {
		for name := range strings.SplitSeq(path.Dir(p), "/") {
			if strings.HasPrefix(name, ".") && name != sign.SignatureDir {
				return false
			}
		}

		return !strings.HasPrefix(path.Base(p), ".")
	}

	files := map[string][]byte{}

	for _, p := range exampleFiles(t, "registry/modules", published) {
		data, err := os.ReadFile(filepath.Join(examplesDir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}

		files[strings.TrimPrefix(p, "registry/")] = data
	}

	who := object.Signature{
		Name: "Example Registry", Email: "registry@example.org", When: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	commit := storeObject(t, r.Storer, &object.Commit{
		Author: who, Committer: who, Message: "The modules of the example registry\n", TreeHash: storeTree(t, r.Storer, files),
	})

	head, err := r.Storer.Reference(plumbing.HEAD)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Storer.SetReference(plumbing.NewHashReference(head.Target(), commit)); err != nil {
		t.Fatal(err)
	}

	// Annotated tags, as rigger module publish makes them.
	for _, m := range mods {
		opts := &git.CreateTagOptions{Tagger: &who, Message: m.name + " " + m.version}
		if _, err := r.CreateTag(m.name+"/v"+m.version, commit, opts); err != nil {
			t.Fatal(err)
		}
	}

	return exampleRegistry{dir: dir, inner: fetch.New(t.TempDir())}
}

// encoder is an object of git that writes itself.
type encoder interface {
	Encode(o plumbing.EncodedObject) error
}

// storeObject stores a commit or a tree and returns its hash.
func storeObject(t *testing.T, s storer.EncodedObjectStorer, o encoder) plumbing.Hash {
	t.Helper()

	obj := s.NewEncodedObject()
	if err := o.Encode(obj); err != nil {
		t.Fatal(err)
	}

	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// storeBlob stores the content of a file and returns its hash.
func storeBlob(t *testing.T, s storer.EncodedObjectStorer, data []byte) plumbing.Hash {
	t.Helper()

	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)

	w, err := obj.Writer()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}

	return h
}

// storeTree stores the tree of files, given by their paths in it, and
// returns its hash. Every file is a regular file.
func storeTree(t *testing.T, s storer.EncodedObjectStorer, files map[string][]byte) plumbing.Hash {
	t.Helper()

	var entries []object.TreeEntry

	dirs := map[string]map[string][]byte{}

	for p, data := range files {
		name, rest, inDir := strings.Cut(p, "/")
		if !inDir {
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: storeBlob(t, s, data)})

			continue
		}

		if dirs[name] == nil {
			dirs[name] = map[string][]byte{}
		}

		dirs[name][rest] = data
	}

	for name, sub := range dirs {
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: storeTree(t, s, sub)})
	}

	sort.Sort(object.TreeEntrySorter(entries))

	return storeObject(t, s, &object.Tree{Entries: entries})
}

// exampleEngine returns an engine that fetches from the example registry.
// The signatures of the registry must hold first; with -update they are
// written again when they do not.
func exampleEngine(t *testing.T) *rigger.Engine {
	t.Helper()

	mods := registryModules(t, engine(t))
	keyFiles := exampleFiles(t, ".", func(p string) bool { return path.Base(p) == exampleKeyFile })

	if stale := staleSignatures(t, mods, keyFiles); len(stale) > 0 {
		if !*updateExamples {
			t.Fatalf("the signatures of examples/registry do not hold:\n  %s\n%s", strings.Join(stale, "\n  "), updateHint)
		}

		signExampleRegistry(t, mods, keyFiles)
	}

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.DiscardHandler)), rigger.WithFetcher(newExampleRegistry(t, mods)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

// A stack of examples/ is a stack rigger stack sync takes, and its lock file
// is the file that sync writes (design 8.3): the versions the constraints of
// the stack resolve to in examples/registry, the commit and the tag of
// each, its tree digest, and the keys of the trust of the stack that sign
// it. The sync is the one of the engine, with the fetcher of Rigger over a
// git repository; only the address of the registry, which names no
// repository of the network, is led to that repository
// (exampleRegistry). With the lock file the stack loads as rigger stack
// validate loads it, without a diagnostic.
//
// The stacks of notSynced are left out, each for its reason.
func TestSyncStack_Examples(t *testing.T) {
	t.Parallel()

	e := exampleEngine(t)
	stacks := exampleStacks(t)

	for dir := range notSynced {
		if !slices.Contains(stacks, dir) {
			t.Errorf("notSynced names %s, which is no stack of examples/", dir)
		}
	}

	for _, dir := range stacks {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			if reason, ok := notSynced[dir]; ok {
				t.Skip("not synced: " + reason)
			}

			lockFile := path.Join(dir, "rigger.lock.hcl")

			if *updateExamples {
				updateExampleLock(t, e, dir, lockFile)
			}

			committed, err := os.ReadFile(filepath.Join(examplesDir, filepath.FromSlash(lockFile)))
			if err != nil {
				t.Fatalf("%v\n%s", err, updateHint)
			}

			// A sync that keeps what the lock file has, and one that resolves
			// every constraint again: both write the file that is there.
			for _, upgrade := range []bool{false, true} {
				_, lock, diags := e.SyncStack(t.Context(), os.DirFS(examplesDir), dir, upgrade)
				for _, d := range diags {
					t.Errorf("SyncStack(upgrade %t): %s", upgrade, d)
				}

				if !diags.HasErrors() && !bytes.Equal(lock, committed) {
					t.Errorf("SyncStack(upgrade %t) writes another lock file than examples/%s:\n%s\nthe file:\n%s\n%s",
						upgrade, lockFile, lock, committed, updateHint)
				}
			}

			_, diags := e.LoadStack(t.Context(), os.DirFS(examplesDir), dir)
			for _, d := range diags {
				t.Errorf("LoadStack(): %s", d)
			}
		})
	}
}

// updateExampleLock writes the lock file of a stack of examples/ again: the
// one a sync writes for the stack without a lock file.
func updateExampleLock(t *testing.T, e *rigger.Engine, dir, lockFile string) {
	t.Helper()

	if err := os.Remove(filepath.Join(examplesDir, filepath.FromSlash(lockFile))); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	_, lock, diags := e.SyncStack(t.Context(), os.DirFS(examplesDir), dir, false)
	if diags.HasErrors() {
		t.Fatalf("SyncStack(): %s", diags)
	}

	writeExample(t, lockFile, lock)
}
