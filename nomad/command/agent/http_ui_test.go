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

func TestUIAssetWrapper_Fallback(t *testing.T) {
	fs := testUIAssets(map[string]string{
		"index.html":    "new",
		"assets/app.js": "new-js",
	})

	cases := []struct {
		path string
		want string
	}{
		{"/assets/app.js", "new-js"},
		{"/jobs/example", "new"},
		// the previous UI is gone: its old links reach the not-found page of
		// the UI
		{"/legacy/jobs", "new"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			must.Eq(t, tc.want, readUIAsset(t, fs, tc.path))
		})
	}
}
