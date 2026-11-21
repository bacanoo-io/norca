// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package ocitest is an OCI registry in memory for tests: blobs, manifests
// and tags of the distribution API, behind a bearer token.
package ocitest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// token is the bearer token the registry gives and asks for.
const token = "ocitest-token"

// Registry is an OCI registry in memory.
type Registry struct {
	*httptest.Server

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte // by repository and tag or digest
	uploads   int
}

// New starts a registry; Close stops it.
func New() *Registry {
	r := &Registry{blobs: map[string][]byte{}, manifests: map[string][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))

	return r
}

// Host is the host of the registry: 127.0.0.1:<port>.
func (r *Registry) Host() string {
	return strings.TrimPrefix(r.URL, "http://")
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})

		return
	}

	if req.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.URL+`/token",service="ocitest"`)
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	path := strings.TrimPrefix(req.URL.Path, "/v2/")

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case strings.Contains(path, "/blobs/uploads/") && req.Method == http.MethodPost:
		r.uploads++

		w.Header().Set("Location", "/v2/"+strings.SplitN(path, "/blobs/", 2)[0]+"/blobs/uploads/u"+strconv.Itoa(r.uploads))
		w.WriteHeader(http.StatusAccepted)
	case strings.Contains(path, "/blobs/uploads/") && req.Method == http.MethodPut:
		data, _ := io.ReadAll(req.Body)
		if digestOf(data) != req.URL.Query().Get("digest") {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		r.blobs[digestOf(data)] = data

		w.WriteHeader(http.StatusCreated)
	case strings.Contains(path, "/blobs/"):
		data, ok := r.blobs[path[strings.LastIndex(path, "/")+1:]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if req.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case strings.Contains(path, "/manifests/"):
		r.manifest(w, req, path)
	case strings.HasSuffix(path, "/tags/list"):
		r.tags(w, strings.TrimSuffix(path, "/tags/list"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (r *Registry) manifest(w http.ResponseWriter, req *http.Request, path string) {
	repo, ref, _ := strings.Cut(path, "/manifests/")

	if req.Method == http.MethodPut {
		data, _ := io.ReadAll(req.Body)
		r.manifests[repo+":"+ref] = data
		r.manifests[repo+"@"+digestOf(data)] = data

		w.WriteHeader(http.StatusCreated)

		return
	}

	data, ok := r.manifests[repo+":"+ref]
	if !ok {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	w.Header().Set("Docker-Content-Digest", digestOf(data))
	_, _ = w.Write(data)
}

func (r *Registry) tags(w http.ResponseWriter, repo string) {
	var tags []string

	for k := range r.manifests {
		if name, tag, ok := strings.Cut(k, ":"); ok && name == repo {
			tags = append(tags, tag)
		}
	}

	slices.Sort(tags)
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
}
