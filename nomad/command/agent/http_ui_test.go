// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"io"
	"testing"

	assetfs "github.com/elazarl/go-bindata-assetfs"
	"github.com/shoenig/test/must"
)

func testUIAssets(files map[string]string) *UIAssetWrapper {
	return &UIAssetWrapper{FileSystem: &assetfs.AssetFS{
		Asset: func(name string) ([]byte, error) {
			if content, ok := files[name]; ok {
				return []byte(content), nil
			}
			return nil, fmt.Errorf("Asset %s not found", name)
		},
		AssetDir: func(name string) ([]string, error) {
			return nil, fmt.Errorf("Asset %s not found", name)
		},
	}}
}

func readUIAsset(t *testing.T, fs *UIAssetWrapper, name string) string {
	t.Helper()
	f, err := fs.Open(name)
	must.NoError(t, err)
	defer f.Close()
	b, err := io.ReadAll(f)
	must.NoError(t, err)
	return string(b)
}

func TestUIAssetWrapper_LegacyFallback(t *testing.T) {
	fs := testUIAssets(map[string]string{
		"index.html":            "new",
		"assets/app.js":         "new-js",
		"legacy/index.html":     "legacy",
		"legacy/assets/app.js":  "legacy-js",
		"legacy/robots.txt":     "legacy-robots",
		"legacyish/unrelated.x": "x",
	})

	cases := []struct {
		path string
		want string
	}{
		{"/assets/app.js", "new-js"},
		{"/jobs/example", "new"},
		{"/legacy/assets/app.js", "legacy-js"},
		{"/legacy/jobs/example", "legacy"},
		{"legacy/clients", "legacy"},
		{"/legacyish/jobs", "new"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			must.Eq(t, tc.want, readUIAsset(t, fs, tc.path))
		})
	}
}

func TestUIAssetWrapper_NoLegacyBuild(t *testing.T) {
	// A binary built without the legacy UI serves the new UI's index.html,
	// whose not-found page handles the unknown route.
	fs := testUIAssets(map[string]string{"index.html": "new"})
	must.Eq(t, "new", readUIAsset(t, fs, "/legacy/jobs"))
}
