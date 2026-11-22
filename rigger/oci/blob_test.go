// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The digest of a layer is the manifest's word: it names a blob, no other
// path of the registry.
func TestClient_BlobDigest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	repository := strings.TrimPrefix(srv.URL, "http://") + "/acme/web"
	if _, err := NewClient().Blob(t.Context(), repository, "../../../../admin"); err == nil {
		t.Error("Blob() of what is not a digest = nil")
	}

	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests were sent for what is not a digest", n)
	}
}
