// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package ocitest provides an in-process OCI registry for tests. It
// implements the parts of the OCI distribution specification that pack
// clients use: blob upload and download, manifests by tag and digest, tag
// listing and the catalog, with optional basic authentication and TLS.
package ocitest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry is an in-memory OCI registry.
type Registry struct {
	// Username and Password, if set, are required as basic authentication.
	Username string
	Password string

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]map[string]manifest // repository -> digest -> manifest
	tags      map[string]map[string]string   // repository -> tag -> digest
	uploads   map[string]*bytes.Buffer
	nextID    int

	// Requests counts the handled requests.
	Requests atomic.Int64
}

type manifest struct {
	mediaType string
	data      []byte
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		blobs:     map[string][]byte{},
		manifests: map[string]map[string]manifest{},
		tags:      map[string]map[string]string{},
		uploads:   map[string]*bytes.Buffer{},
	}
}

// Server is a running registry.
type Server struct {
	*Registry
	*httptest.Server
}

// Host returns the "host:port" of the server.
func (s *Server) Host() string {
	u, _ := url.Parse(s.URL)
	return u.Host
}

// Start starts the registry as a plain HTTP server.
func (r *Registry) Start() *Server {
	return &Server{Registry: r, Server: httptest.NewServer(r)}
}

// StartTLS starts the registry as a HTTPS server with a test certificate,
// available as Server.Certificate().
func (r *Registry) StartTLS() *Server {
	return &Server{Registry: r, Server: httptest.NewTLSServer(r)}
}

// Tags returns the tags of a repository.
func (r *Registry) Tags(repo string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var tags []string
	for t := range r.tags[repo] {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags
}

// Manifest returns the manifest with the given tag or digest.
func (r *Registry) Manifest(repo, ref string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.lookup(repo, ref)
	return m.data, ok
}

// PutManifest stores a manifest directly, e.g. to tamper with an artifact.
func (r *Registry) PutManifest(repo, tag, mediaType string, data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.putManifest(repo, tag, mediaType, data)
}

// PutBlob stores a blob directly.
func (r *Registry) PutBlob(data []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digestOf(data)
	r.blobs[d] = data
	return d
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func (r *Registry) lookup(repo, ref string) (manifest, bool) {
	if !strings.Contains(ref, ":") {
		d, ok := r.tags[repo][ref]
		if !ok {
			return manifest{}, false
		}
		ref = d
	}
	m, ok := r.manifests[repo][ref]
	return m, ok
}

func (r *Registry) putManifest(repo, tag, mediaType string, data []byte) string {
	d := digestOf(data)
	if r.manifests[repo] == nil {
		r.manifests[repo] = map[string]manifest{}
	}
	r.manifests[repo][d] = manifest{mediaType: mediaType, data: data}
	if tag != "" && !strings.Contains(tag, ":") {
		if r.tags[repo] == nil {
			r.tags[repo] = map[string]string{}
		}
		r.tags[repo][tag] = d
	}
	return d
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": msg}},
	})
}

// ServeHTTP implements the distribution API.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.Requests.Add(1)
	if r.Username != "" || r.Password != "" {
		u, p, ok := req.BasicAuth()
		if !ok || u != r.Username || p != r.Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="ocitest"`)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
	}

	p := req.URL.Path
	if p == "/v2/" || p == "/v2" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !strings.HasPrefix(p, "/v2/") {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "not found")
		return
	}
	p = strings.TrimPrefix(p, "/v2/")

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case p == "_catalog":
		r.catalog(w)
	case strings.HasSuffix(p, "/tags/list"):
		r.tagList(w, strings.TrimSuffix(p, "/tags/list"))
	case strings.Contains(p, "/manifests/"):
		repo, ref, _ := cutLast(p, "/manifests/")
		r.manifest(w, req, repo, ref)
	case strings.Contains(p, "/blobs/uploads"):
		repo, id, _ := cutLast(p, "/blobs/uploads")
		r.upload(w, req, repo, strings.TrimPrefix(id, "/"))
	case strings.Contains(p, "/blobs/"):
		_, d, _ := cutLast(p, "/blobs/")
		r.blob(w, req, d)
	default:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "not found")
	}
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func (r *Registry) catalog(w http.ResponseWriter) {
	repos := []string{}
	for repo := range r.manifests {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
}

func (r *Registry) tagList(w http.ResponseWriter, repo string) {
	tags := []string{}
	for t := range r.tags[repo] {
		tags = append(tags, t)
	}
	if len(tags) == 0 && r.manifests[repo] == nil {
		writeError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not found")
		return
	}
	sort.Strings(tags)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
}

func (r *Registry) manifest(w http.ResponseWriter, req *http.Request, repo, ref string) {
	switch req.Method {
	case http.MethodGet, http.MethodHead:
		m, ok := r.lookup(repo, ref)
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		w.Header().Set("Content-Type", m.mediaType)
		w.Header().Set("Docker-Content-Digest", digestOf(m.data))
		w.Header().Set("Content-Length", strconv.Itoa(len(m.data)))
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(m.data)
		}
	case http.MethodPut:
		data, err := io.ReadAll(req.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		if strings.Contains(ref, ":") && ref != digestOf(data) {
			writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
			return
		}
		var m struct {
			Config struct {
				Digest string `json:"digest"`
			} `json:"config"`
			Layers []struct {
				Digest string `json:"digest"`
			} `json:"layers"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			writeError(w, http.StatusBadRequest, "MANIFEST_INVALID", err.Error())
			return
		}
		for _, d := range append([]string{m.Config.Digest}, layerDigests(m.Layers)...) {
			if _, ok := r.blobs[d]; d != "" && !ok {
				writeError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "blob unknown: "+d)
				return
			}
		}
		d := r.putManifest(repo, ref, req.Header.Get("Content-Type"), data)
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/manifests/%s", repo, d))
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		m, ok := r.lookup(repo, ref)
		if !ok {
			writeError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		d := digestOf(m.data)
		delete(r.manifests[repo], d)
		for t, td := range r.tags[repo] {
			if td == d {
				delete(r.tags[repo], t)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func layerDigests(layers []struct {
	Digest string `json:"digest"`
}) []string {
	var out []string
	for _, l := range layers {
		out = append(out, l.Digest)
	}
	return out
}

func (r *Registry) blob(w http.ResponseWriter, req *http.Request, d string) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		return
	}
	b, ok := r.blobs[d]
	if !ok {
		writeError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", d)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodGet {
		_, _ = w.Write(b)
	}
}

func (r *Registry) upload(w http.ResponseWriter, req *http.Request, repo, id string) {
	q := req.URL.Query()
	switch {
	case req.Method == http.MethodPost && id == "":
		if mount := q.Get("mount"); mount != "" {
			if _, ok := r.blobs[mount]; ok {
				w.Header().Set("Docker-Content-Digest", mount)
				w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, mount))
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		if d := q.Get("digest"); d != "" {
			data, _ := io.ReadAll(req.Body)
			r.finishUpload(w, repo, d, data)
			return
		}
		r.nextID++
		uid := strconv.Itoa(r.nextID)
		r.uploads[uid] = &bytes.Buffer{}
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, uid))
		w.Header().Set("Docker-Upload-UUID", uid)
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPatch && id != "":
		buf, ok := r.uploads[id]
		if !ok {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
			return
		}
		_, _ = io.Copy(buf, req.Body)
		w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/uploads/%s", repo, id))
		w.Header().Set("Range", fmt.Sprintf("0-%d", buf.Len()-1))
		w.WriteHeader(http.StatusAccepted)
	case req.Method == http.MethodPut && id != "":
		buf, ok := r.uploads[id]
		if !ok {
			writeError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
			return
		}
		_, _ = io.Copy(buf, req.Body)
		delete(r.uploads, id)
		r.finishUpload(w, repo, q.Get("digest"), buf.Bytes())
	default:
		writeError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

func (r *Registry) finishUpload(w http.ResponseWriter, repo, d string, data []byte) {
	if d != digestOf(data) {
		writeError(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
		return
	}
	r.blobs[d] = data
	w.Header().Set("Docker-Content-Digest", d)
	w.Header().Set("Location", fmt.Sprintf("/v2/%s/blobs/%s", repo, d))
	w.WriteHeader(http.StatusCreated)
}
