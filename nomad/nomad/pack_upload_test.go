// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

// packArchive returns a tar (gzipped if gz) of the files, by name.
func packArchive(t *testing.T, files map[string][]byte, gz bool) []byte {
	var buf bytes.Buffer
	var w *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = tar.NewWriter(zw)
	} else {
		w = tar.NewWriter(&buf)
	}
	for name, content := range files {
		must.NoError(t, w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
		_, err := w.Write(content)
		must.NoError(t, err)
	}
	must.NoError(t, w.Close())
	if zw != nil {
		must.NoError(t, zw.Close())
	}
	return buf.Bytes()
}

// helloPackFiles is the hello pack of writeDirPack below a directory.
func helloPackFiles(t *testing.T, version string) map[string][]byte {
	dir := t.TempDir()
	writeDirPack(t, dir, version)
	out := map[string][]byte{}
	must.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		out["hello/"+filepath.ToSlash(rel)] = b
		return err
	}))
	return out
}

func TestPacks_Upload(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, func(c *Config) {
		c.ReleaseFromRendered = func(name string, rendered *api.ReleaseApplyRequest, namespace string) (*structs.ReleaseApplyRequest, error) {
			return &structs.ReleaseApplyRequest{Name: name, Values: rendered.Values}, nil
		}
	})
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	codec := rpcClient(t, s)
	write := structs.WriteRequest{Region: "global"}
	upload := func(registry, file string, data []byte) (*PackUploadResponse, error) {
		var resp PackUploadResponse
		err := msgpackrpc.CallWithCodec(codec, "Packs.Upload", &PackUploadRequest{
			Registry: registry, FileName: file, Data: data, WriteRequest: write,
		}, &resp)
		return &resp, err
	}

	// The first upload creates the registry "uploads".
	v1 := packArchive(t, helloPackFiles(t, "1.0.0"), true)
	resp, err := upload("", "hello-1.0.0.tgz", v1)
	must.NoError(t, err)
	must.Eq(t, DefaultUploadRegistry, resp.Registry.Name)
	must.Eq(t, PackRegistryTypeUploads, resp.Registry.Spec.Type)
	must.Eq(t, PackRegistryPhaseReady, resp.Registry.Status.Phase, must.Sprint(resp.Registry.Status.Message))
	must.Len(t, 1, resp.Registry.Status.Packs)
	must.Eq(t, "hello", resp.Registry.Status.Packs[0].Name)
	must.Len(t, 1, resp.Registry.Status.Uploads)
	must.Eq(t, []string{"hello@1.0.0"}, resp.Registry.Status.Uploads[0].Packs)
	must.Eq(t, "hello-1.0.0.tgz", resp.Registry.Status.Uploads[0].FileName)
	digest1 := resp.Digest

	// The same archive again is stored once.
	resp, err = upload("", "again.tgz", v1)
	must.NoError(t, err)
	must.Len(t, 1, resp.Registry.Status.Uploads)

	// A larger, uncompressed archive is stored in chunks.
	files := helloPackFiles(t, "1.1.0")
	big := make([]byte, 400*1024)
	rand.New(rand.NewSource(1)).Read(big)
	files["hello/templates/blob.bin"] = big
	resp, err = upload("", "hello-1.1.0.tar", packArchive(t, files, false))
	must.NoError(t, err)
	must.Len(t, 2, resp.Registry.Status.Uploads)
	must.Len(t, 2, resp.Registry.Status.Packs)
	snap, _ := s.State().Snapshot()
	chunks, err := uploadObjects(snap, DefaultUploadRegistry)
	must.NoError(t, err)
	must.Eq(t, 1+3, len(chunks), must.Sprint("one chunk for v1, three for v1.1"))

	// Installing from the uploads registry renders on the leader.
	var applied structs.ReleaseApplyResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Deploy", &PackDeployRPCRequest{
		Name: "hello", DryRun: true, WriteRequest: write,
		Deploy: &PackDeployRequest{Registry: DefaultUploadRegistry, Pack: "hello", Version: "1.0.0"},
	}, &applied))

	// A new leader (no cache) rebuilds the registry from the chunks.
	cacheDir, err := s.releases.packCachePath()
	must.NoError(t, err)
	must.NoError(t, os.RemoveAll(filepath.Join(cacheDir, DefaultUploadRegistry)))
	var detail PackDetailRPCResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Detail", &PackDetailRequest{
		Registry: DefaultUploadRegistry, Pack: "hello", Version: "1.1.0", QueryOptions: structs.QueryOptions{Region: "global"},
	}, &detail))
	must.Eq(t, "1.1.0", detail.Detail.Pack.Version)
	_, err = os.Stat(filepath.Join(cacheDir, DefaultUploadRegistry, "1.1.0", "hello@1.1.0", "templates", "blob.bin"))
	must.NoError(t, err)

	// Removing an upload removes its packs.
	var deleted PackUploadResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.DeleteUpload", &PackUploadDeleteRequest{
		Registry: DefaultUploadRegistry, Digest: digest1, WriteRequest: write,
	}, &deleted))
	must.Len(t, 1, deleted.Registry.Status.Packs)
	must.Eq(t, "1.1.0", deleted.Registry.Status.Packs[0].Version)

	// Archives that leave their directory, carry no pack, or go to another
	// registry type are refused.
	_, err = upload("", "evil.tgz", packArchive(t, map[string][]byte{"../evil/metadata.hcl": []byte("x")}, true))
	must.ErrorContains(t, err, "leaves the archive")
	_, err = upload("", "empty.tgz", packArchive(t, map[string][]byte{"README.md": []byte("x")}, true))
	must.ErrorContains(t, err, "no pack")
	_, err = upload("", "junk.bin", []byte("not an archive"))
	must.Error(t, err)

	var reg PackRegistryResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry:     &PackRegistry{Name: "builtin", Spec: &PackRegistrySpec{Type: "dir", Source: BuiltinRegistrySource}},
		WriteRequest: write,
	}, &reg))
	_, err = upload("builtin", "hello.tgz", v1)
	must.ErrorContains(t, err, `of the type "dir"`)

	// A registry with trusted keys takes signed bundles only.
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Upsert", &PackRegistryUpsertRequest{
		Registry: &PackRegistry{Name: "signed", Spec: &PackRegistrySpec{Type: PackRegistryTypeUploads,
			TrustedKeys: []string{"-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----"}}},
		WriteRequest: write,
	}, &reg))
	_, err = upload("signed", "hello.tgz", v1)
	must.ErrorContains(t, err, "signed bundle")

	// Deleting the registry deletes its uploads.
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Packs.Delete", &PackRegistryNameRequest{Name: DefaultUploadRegistry, WriteRequest: write}, &reg))
	snap, _ = s.State().Snapshot()
	chunks, err = uploadObjects(snap, DefaultUploadRegistry)
	must.NoError(t, err)
	must.SliceEmpty(t, chunks)
}
