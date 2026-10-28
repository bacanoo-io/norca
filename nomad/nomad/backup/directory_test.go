// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testDirDestination(t *testing.T) (*dirDestination, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := newDirDestination(&DestinationConfig{Name: "local", Type: DestinationTypeDirectory, Path: dir}, "eu")
	must.NoError(t, err)
	return d, dir
}

// files returns the files below a directory, slash-separated and sorted.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	must.NoError(t, err)
	slices.Sort(out)
	return out
}

type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection lost")
	}
	n := min(len(p), r.n)
	r.n -= n
	return n, nil
}

func TestDirDestination_Objects(t *testing.T) {
	ci.Parallel(t)
	d, dir := testDirDestination(t)
	ctx := t.Context()

	// The default prefix, below the path.
	must.Eq(t, filepath.Join(dir, "norca", "eu"), d.root)
	must.StrHasPrefix(t, "file://", d.Location("2026/10/24/x/"))
	must.StrHasSuffix(t, "/norca/eu/2026/10/24/x/", d.Location("2026/10/24/x/"))

	// Put with a known and an unknown size, Get and Size.
	must.NoError(t, d.Put(ctx, "2026/10/24/a/state.snap.age", strings.NewReader("state"), 5, ""))
	must.NoError(t, d.Put(ctx, "2026/10/24/a/manifest.json", strings.NewReader("{}"), -1, ""))
	must.NoError(t, d.Put(ctx, "latest.json", strings.NewReader("1"), -1, ""))
	size, err := d.Size(ctx, "2026/10/24/a/state.snap.age")
	must.NoError(t, err)
	must.Eq(t, int64(5), size)
	rc, err := d.Get(ctx, "2026/10/24/a/state.snap.age")
	must.NoError(t, err)
	got, err := io.ReadAll(rc)
	must.NoError(t, err)
	must.NoError(t, rc.Close())
	must.Eq(t, "state", string(got))

	// Overwriting replaces the object.
	must.NoError(t, d.Put(ctx, "latest.json", strings.NewReader("22"), -1, ""))
	size, err = d.Size(ctx, "latest.json")
	must.NoError(t, err)
	must.Eq(t, int64(2), size)

	// Missing objects are ErrNotFound with their location.
	_, err = d.Get(ctx, "2026/10/24/b/state.snap.age")
	must.ErrorIs(t, err, ErrNotFound)
	must.ErrorContains(t, err, "file://")
	_, err = d.Size(ctx, "nope")
	must.ErrorIs(t, err, ErrNotFound)
	// A folder is not an object.
	_, err = d.Get(ctx, "2026/10/24/a")
	must.Error(t, err)

	// Permissions: files 0600, directories 0700.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(d.root, "2026", "10", "24", "a", "state.snap.age"))
		must.NoError(t, err)
		must.Eq(t, fs.FileMode(0o600), info.Mode().Perm())
		info, err = os.Stat(filepath.Join(d.root, "2026", "10", "24", "a"))
		must.NoError(t, err)
		must.Eq(t, fs.FileMode(0o700), info.Mode().Perm())
	}

	// A wrong size and a failing reader leave neither the object nor a
	// temporary file behind.
	err = d.Put(ctx, "2026/10/24/a/keyring.age", strings.NewReader("abc"), 5, "")
	must.ErrorContains(t, err, "expected 5")
	err = d.Put(ctx, "2026/10/24/a/keyring.age", &failingReader{n: 100}, -1, "")
	must.ErrorContains(t, err, "connection lost")
	must.Eq(t, []string{
		"norca/eu/2026/10/24/a/manifest.json",
		"norca/eu/2026/10/24/a/state.snap.age",
		"norca/eu/latest.json",
	}, files(t, dir))

	// DeletePrefix removes the folder and the empty day, month and year
	// directories, not the root.
	must.NoError(t, d.Put(ctx, "2026/10/25/b/manifest.json", strings.NewReader("{}"), -1, ""))
	must.NoError(t, d.DeletePrefix(ctx, "2026/10/24/a/"))
	must.Eq(t, []string{"norca/eu/2026/10/25/b/manifest.json", "norca/eu/latest.json"}, files(t, dir))
	_, err = os.Stat(filepath.Join(d.root, "2026", "10", "24"))
	must.True(t, errors.Is(err, fs.ErrNotExist))
	must.NoError(t, d.DeletePrefix(ctx, "2026/10/25/b/"))
	must.Eq(t, []string{"norca/eu/latest.json"}, files(t, dir))
	_, err = os.Stat(filepath.Join(d.root, "2026"))
	must.True(t, errors.Is(err, fs.ErrNotExist))

	// Deleting a missing folder is not an error.
	must.NoError(t, d.DeletePrefix(ctx, "2026/01/01/x/"))
	_, err = os.Stat(d.root)
	must.NoError(t, err)
}

func TestDirDestination_CrashSafe(t *testing.T) {
	ci.Parallel(t)
	d, dir := testDirDestination(t)
	ctx := t.Context()

	// An interrupted write leaves the previous object intact: only its
	// temporary file is incomplete.
	must.NoError(t, d.Put(ctx, "2026/10/24/a/manifest.json", strings.NewReader("old"), -1, ""))
	folder := filepath.Join(d.root, "2026", "10", "24", "a")
	tmp, err := os.CreateTemp(folder, ".manifest.json"+tmpMarker+"*")
	must.NoError(t, err)
	_, err = tmp.WriteString("ne")
	must.NoError(t, err)
	must.NoError(t, tmp.Close())
	tmp2 := filepath.Join(folder, ".state.snap.age"+tmpMarker+"123")
	must.NoError(t, os.WriteFile(tmp2, []byte("partial"), 0o600))
	// A file of another program that looks similar is left alone.
	must.NoError(t, os.WriteFile(filepath.Join(folder, "notes.txt"), []byte("x"), 0o600))

	rc, err := d.Get(ctx, "2026/10/24/a/manifest.json")
	must.NoError(t, err)
	got, _ := io.ReadAll(rc)
	rc.Close()
	must.Eq(t, "old", string(got))

	// AbortIncompleteUploads removes the temporary files.
	n, err := d.AbortIncompleteUploads(ctx, "")
	must.NoError(t, err)
	must.Eq(t, 2, n)
	must.Eq(t, []string{
		"norca/eu/2026/10/24/a/manifest.json",
		"norca/eu/2026/10/24/a/notes.txt",
	}, files(t, dir))

	// Nothing to abort in a missing directory.
	d2, err := newDirDestination(&DestinationConfig{Name: "x", Type: DestinationTypeDirectory,
		Path: filepath.Join(dir, "missing")}, "eu")
	must.NoError(t, err)
	n, err = d2.AbortIncompleteUploads(ctx, "")
	must.NoError(t, err)
	must.Zero(t, n)
}

func TestDirDestination_Containment(t *testing.T) {
	ci.Parallel(t)
	d, dir := testDirDestination(t)
	ctx := t.Context()

	// A file next to the destination's root must survive everything below.
	must.NoError(t, os.MkdirAll(d.root, 0o700))
	outside := filepath.Join(dir, "norca", "keep.txt")
	must.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))

	for _, key := range []string{"../keep.txt", "a/../../keep.txt", "..", `..\keep.txt`, `a\..\..\keep.txt`} {
		must.Error(t, d.Put(ctx, key, strings.NewReader("y"), -1, ""), must.Sprint(key))
		_, err := d.Get(ctx, key)
		must.Error(t, err, must.Sprint(key))
		must.Error(t, d.DeletePrefix(ctx, key), must.Sprint(key))
		must.Error(t, d.DeletePrefix(ctx, key+"/"), must.Sprint(key))
		_, err = d.AbortIncompleteUploads(ctx, key)
		must.Error(t, err, must.Sprint(key))
	}

	// Deleting everything under the root keeps the root and what is next
	// to it.
	must.NoError(t, d.Put(ctx, "2026/10/24/a/manifest.json", strings.NewReader("{}"), -1, ""))
	must.NoError(t, d.DeletePrefix(ctx, ""))
	_, err := os.Stat(d.root)
	must.NoError(t, err)
	must.Eq(t, []string{"norca/keep.txt"}, files(t, dir))

	// A key prefix that is not a folder deletes matching names only.
	must.NoError(t, d.Put(ctx, "2026/10/24/ab/manifest.json", strings.NewReader("{}"), -1, ""))
	must.NoError(t, d.Put(ctx, "2026/10/24/b/manifest.json", strings.NewReader("{}"), -1, ""))
	must.NoError(t, d.DeletePrefix(ctx, "2026/10/24/a"))
	must.Eq(t, []string{"norca/eu/2026/10/24/b/manifest.json", "norca/keep.txt"}, files(t, dir))

	// A prefix with ".." is refused by the configuration and the
	// constructor.
	cfg := &DestinationConfig{Name: "x", Type: DestinationTypeDirectory, Path: dir, Prefix: "norca/../.."}
	must.ErrorContains(t, cfg.Validate(), `".."`)
	_, err = newDirDestination(cfg, "eu")
	must.Error(t, err)
	_, err = newDirDestination(&DestinationConfig{Name: "x", Type: DestinationTypeDirectory, Prefix: "/"}, "eu")
	must.ErrorContains(t, err, "absolute")

	// Pre-signed URLs don't exist for a directory.
	_, err = d.PresignGet(ctx, "2026/10/24/b/manifest.json", time.Minute)
	must.ErrorIs(t, err, ErrNoDownloadURL)
	must.ErrorContains(t, err, "file://")
}

func TestFileURL(t *testing.T) {
	ci.Parallel(t)
	if runtime.GOOS == "windows" {
		must.Eq(t, "file:///C:/backups/norca", fileURL(`C:\backups\norca`))
		must.Eq(t, "file://server/share/norca", fileURL(`\\server\share\norca`))
		return
	}
	must.Eq(t, "file:///var/backups/norca", fileURL("/var/backups/norca"))
}

func TestDestinationConfig_Validate(t *testing.T) {
	ci.Parallel(t)

	abs := "/var/backups/norca"
	if runtime.GOOS == "windows" {
		abs = `C:\backups\norca`
	}
	cases := []struct {
		name string
		cfg  DestinationConfig
		errs []string
	}{
		{"s3", DestinationConfig{Name: "a", Type: "s3", Bucket: "b"}, nil},
		{"directory", DestinationConfig{Name: "a", Type: "directory", Path: abs, Prefix: "norca/${region}"}, nil},
		{"unknown type", DestinationConfig{Name: "a", Type: "azure", Bucket: "b"},
			[]string{`unsupported type "azure", must be "s3" or "directory"`}},
		{"s3 without bucket", DestinationConfig{Name: "a", Type: "s3"}, []string{"missing bucket"}},
		{"path on s3", DestinationConfig{Name: "a", Type: "s3", Bucket: "b", Path: abs},
			[]string{`path is only supported by type "directory"`}},
		{"directory without path", DestinationConfig{Name: "a", Type: "directory"}, []string{"missing path"}},
		{"relative path", DestinationConfig{Name: "a", Type: "directory", Path: "backups"},
			[]string{`path "backups" must be absolute`}},
		{"s3 keys on a directory", DestinationConfig{Name: "a", Type: "directory", Path: abs,
			Endpoint: "https://s3", Bucket: "b", Region: "eu", ForcePathStyle: true, AccessKey: "k", SecretKey: "s",
			SessionToken: "t", CredentialsFile: "c", Profile: "p", CAFile: "ca", SSE: "AES256", KMSKey: "k"},
			[]string{
				`endpoint is not supported by type "directory"`, `bucket is not supported by type "directory"`,
				`region is not supported by type "directory"`, `force_path_style is not supported by type "directory"`,
				`access_key is not supported by type "directory"`, `secret_key is not supported by type "directory"`,
				`session_token is not supported by type "directory"`, `credentials_file is not supported by type "directory"`,
				`profile is not supported by type "directory"`, `ca_file is not supported by type "directory"`,
				`sse is not supported by type "directory"`, `kms_key is not supported by type "directory"`,
			}},
		{"bad interpolation", DestinationConfig{Name: "a", Type: "directory", Path: abs, Prefix: "${node}"},
			[]string{"prefix may only interpolate ${region}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if len(tc.errs) == 0 {
				must.NoError(t, err)
				return
			}
			for _, e := range tc.errs {
				must.ErrorContains(t, err, e)
			}
		})
	}

	// Both types in one configuration.
	env := newTestEnv(t, 16)
	cfg := env.config("offsite")
	cfg.Destinations = append(cfg.Destinations, &DestinationConfig{
		Name: "local", Type: DestinationTypeDirectory, Path: abs, Prune: true})
	must.NoError(t, cfg.Validate())
}

// dirTestConfig is a configuration with a directory destination "local" and
// an S3 destination "offsite".
func (env *testEnv) dirTestConfig(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := env.config("offsite")
	cfg.Destinations = append([]*DestinationConfig{{
		Name: "local", Type: DestinationTypeDirectory, Path: dir, Prefix: "norca/${region}", Prune: true,
	}}, cfg.Destinations...)
	return cfg, dir
}

func TestManager_Directory(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, minPartSize+4321)
	cfg, dir := env.dirTestConfig(t)
	cfg.Retention = &RetentionConfig{MinKeep: 1}
	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)

	first, err := m.Save(SaveOptions{Pin: true})
	must.NoError(t, err)
	must.Eq(t, structs.BackupStatusComplete, first.Status)
	must.Eq(t, []string{"local", "offsite"}, first.VerifiedDestinations())
	local := first.Destination("local")
	must.Eq(t, fileURL(filepath.Join(dir, "norca", "eu", filepath.FromSlash(first.Path)))+"/", local.Location)

	// The same layout as in S3, and the same objects.
	folder := "norca/eu/" + first.Path + "/"
	must.Eq(t, []string{folder + KeyringKey, folder + ManifestKey, folder + StateKey, "norca/eu/" + LatestKey},
		files(t, dir))
	must.NotNil(t, env.s3.Object("bucket-offsite", folder+ManifestKey))
	stored, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(folder+StateKey)))
	must.NoError(t, err)
	must.Eq(t, local.Objects[structs.BackupObjectState].Size, int64(len(stored)))
	must.False(t, bytes.Contains(stored, env.snapshot[:64]))

	// The record can be rebuilt from the manifest in the directory.
	loaded, err := m.LoadRecord(t.Context(), first.ID, "local")
	must.NoError(t, err)
	must.Eq(t, first.RaftIndex, loaded.RaftIndex)
	must.Eq(t, local.Location, loaded.Destinations[0].Location)

	// Load (browse, selective restore) decrypts from the directory.
	var restored []byte
	restore := func(archive io.Reader) (*state.StateStore, error) {
		var err error
		restored, err = io.ReadAll(archive)
		return state.TestStateStore(t), err
	}
	ld, err := m.Load(t.Context(), first, "local", env.identity.String(), restore)
	must.NoError(t, err)
	must.Eq(t, "local", ld.Destination)
	must.True(t, bytes.Equal(env.snapshot, restored))
	must.Len(t, 1, ld.Keys)

	// The download URL comes from the S3 destination; the directory has
	// none.
	u, err := m.PresignURL(t.Context(), first, "", structs.BackupObjectState, time.Minute)
	must.NoError(t, err)
	must.StrContains(t, u, "X-Amz-Signature")
	_, err = m.PresignURL(t.Context(), first, "local", structs.BackupObjectState, time.Minute)
	must.ErrorIs(t, err, ErrNoDownloadURL)
	must.ErrorContains(t, err, local.Location+StateKey)

	// Retention: with min_keep 1 the second backup expires once a third
	// exists; the pinned one stays.
	second, err := m.Save(SaveOptions{})
	must.NoError(t, err)
	third, err := m.Save(SaveOptions{})
	must.NoError(t, err)
	all := strings.Join(files(t, dir), "\n")
	must.StrContains(t, all, first.ID)
	must.StrNotContains(t, all, second.ID)
	must.StrContains(t, all, third.ID)
	for _, k := range env.s3.Keys("bucket-offsite") {
		must.StrNotContains(t, k, second.ID)
	}

	// Manual deletion.
	unpinned := first.Copy()
	unpinned.Pinned = false
	_, err = m.Update(unpinned)
	must.NoError(t, err)
	rec, err := env.store.BackupByID(nil, first.ID)
	must.NoError(t, err)
	_, err = m.Delete(rec)
	must.NoError(t, err)
	must.StrNotContains(t, strings.Join(files(t, dir), "\n"), first.ID)
}

// TestManager_DirectoryNotShared simulates a leader change to a server that
// does not see the directory of the previous leader: every access to a
// backup it recorded names the shared storage requirement.
func TestManager_DirectoryNotShared(t *testing.T) {
	ci.Parallel(t)
	env := newTestEnv(t, 2048)
	cfg, dir := env.dirTestConfig(t)
	cfg.Destinations = cfg.Destinations[:1]
	cfg.Retention = &RetentionConfig{MinKeep: 1}

	var logs bytes.Buffer
	env.deps.Logger = hclog.New(&hclog.LoggerOptions{Output: &logs, Level: hclog.Warn})
	m := New(cfg, env.deps)
	m.SetLeader(true)
	defer m.SetLeader(false)
	first, err := m.Save(SaveOptions{})
	must.NoError(t, err)

	// The new leader has its own, empty directory at the same path.
	must.NoError(t, os.RemoveAll(filepath.Join(dir, "norca")))

	_, err = m.Load(t.Context(), first, "", env.identity.String(), func(io.Reader) (*state.StateStore, error) {
		return nil, errors.New("unreachable")
	})
	must.ErrorIs(t, err, ErrNotFound)
	must.ErrorContains(t, err, "shared storage")
	must.ErrorContains(t, err, `by server "srv-1"`)

	m.checkSharedStorage()
	must.StrContains(t, logs.String(), "backup missing on a directory destination")
	must.StrContains(t, logs.String(), "shared storage")

	// Retention keeps the record and reports the requirement.
	second, err := m.Save(SaveOptions{})
	must.NoError(t, err)
	must.StrContains(t, logs.String(), "failed to delete backup "+first.ID)
	rec, err := env.store.BackupByID(nil, first.ID)
	must.NoError(t, err)
	must.NotNil(t, rec)

	// A manual deletion removes the record with a warning.
	_, err = m.Delete(rec)
	must.NoError(t, err)
	rec, err = env.store.BackupByID(nil, first.ID)
	must.NoError(t, err)
	must.Nil(t, rec)
	must.StrContains(t, logs.String(), "deleting the record of a backup missing")

	// A missing object of a backup whose folder exists is a plain not found.
	must.NoError(t, os.Remove(filepath.Join(dir, "norca", "eu", filepath.FromSlash(second.Path), KeyringKey)))
	_, err = m.Open(t.Context(), second, "", structs.BackupObjectKeyring, env.identities(t))
	must.ErrorIs(t, err, ErrNotFound)
	must.StrNotContains(t, err.Error(), "shared storage")
}
