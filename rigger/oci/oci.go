// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package oci keeps modules in OCI registries (design 7.1: oci://), over
// the distribution API: a module version is an image manifest of the
// artifact type of Rigger, tagged with the version, with one layer, the tar.gz
// of the directories of the module and its children, signatures included.
// The manifest digest of a tag stands for its commit.
package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
)

// The media types of a module.
const (
	ArtifactType     = "application/vnd.norca.rigger.module.v1"
	LayerType        = "application/vnd.norca.rigger.module.layer.v1.tar+gzip"
	manifestType     = "application/vnd.oci.image.manifest.v1+json"
	emptyType        = "application/vnd.oci.empty.v1+json"
	emptyJSON        = "{}"
	annotationPath   = "rigger.path"
	annotationDigest = "rigger.digest"
	annotationModule = "rigger.module"
	annotationVer    = "org.opencontainers.image.version"
)

// Descriptor is an OCI descriptor.
type Descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Manifest is an OCI image manifest.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	ArtifactType  string            `json:"artifactType"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// Module is what a manifest says of a module version.
type Module struct {
	Name, Version string
	// Path is the directory of the module in the layer; Digest its tree
	// digest.
	Path, Digest string
}

// Client talks to OCI registries.
type Client struct {
	http *http.Client

	mu     sync.Mutex
	tokens map[string]string // by registry and scope
}

// NewClient returns a client. Credentials come from RIGGER_OCI_USERNAME and
// RIGGER_OCI_PASSWORD; without them it is anonymous.
func NewClient() *Client {
	return &Client{http: http.DefaultClient, tokens: map[string]string{}}
}

// ErrRegistry is the error of an answer of a registry.
var ErrRegistry = errors.New("OCI registry")

// ref splits host/name.
func ref(repository string) (host, name string, err error) {
	host, name, ok := strings.Cut(repository, "/")
	if !ok {
		return "", "", fmt.Errorf("%w: %q is not <host>/<repository>", ErrRegistry, repository)
	}

	return host, name, nil
}

// baseURL is the URL of a registry: http for localhost, as docker does,
// https otherwise.
func baseURL(host string) string {
	name := host
	if h, _, ok := strings.Cut(host, ":"); ok {
		name = h
	}

	if name == "localhost" || name == "127.0.0.1" || name == "::1" {
		return "http://" + host
	}

	return "https://" + host
}

// do sends a request, and once more with a token or credentials when the
// registry asks for them.
func (c *Client) do(ctx context.Context, method, host, scope, target string, body []byte, header http.Header) (*http.Response, error) {
	send := func(auth string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}

		maps.Copy(req.Header, header)

		if auth != "" {
			req.Header.Set("Authorization", auth)
		}

		return c.http.Do(req)
	}

	key := host + " " + scope

	c.mu.Lock()
	auth := c.tokens[key]
	c.mu.Unlock()

	resp, err := send(auth)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}

	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()

	auth, err = c.authorize(ctx, challenge, scope)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tokens[key] = auth
	c.mu.Unlock()

	return send(auth)
}

// authorize answers a challenge: Basic with the credentials, or Bearer with
// a token of the realm.
func (c *Client) authorize(ctx context.Context, challenge, scope string) (string, error) {
	user, password := os.Getenv("RIGGER_OCI_USERNAME"), os.Getenv("RIGGER_OCI_PASSWORD")

	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if user == "" {
			return "", fmt.Errorf("%w: the registry asks for credentials (RIGGER_OCI_USERNAME, RIGGER_OCI_PASSWORD)", ErrRegistry)
		}

		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)), nil
	case "bearer":
		return c.token(ctx, parseChallenge(params), scope, user, password)
	default:
		return "", fmt.Errorf("%w: unknown challenge %q", ErrRegistry, challenge)
	}
}

// parseChallenge reads the parameters of a challenge: realm="...",service="...".
func parseChallenge(params string) map[string]string {
	out := map[string]string{}

	for p := range strings.SplitSeq(params, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			out[strings.ToLower(k)] = strings.Trim(v, `"`)
		}
	}

	return out
}

// token gets a bearer token of a realm.
func (c *Client) token(ctx context.Context, params map[string]string, scope, user, password string) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || params["realm"] == "" {
		return "", fmt.Errorf("%w: a bearer challenge without a realm", ErrRegistry)
	}

	q := realm.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}

	q.Set("scope", scope)
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}

	if user != "" {
		req.SetBasicAuth(user, password)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // read only

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: token: %s", ErrRegistry, resp.Status)
	}

	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}

	if t.Token == "" {
		t.Token = t.AccessToken
	}

	return "Bearer " + t.Token, nil
}

// get reads a resource of a repository.
func (c *Client) get(ctx context.Context, repository, path, accept, action string) ([]byte, http.Header, error) {
	host, name, err := ref(repository)
	if err != nil {
		return nil, nil, err
	}

	header := http.Header{}
	if accept != "" {
		header.Set("Accept", accept)
	}

	resp, err := c.do(ctx, http.MethodGet, host, "repository:"+name+":"+action, baseURL(host)+"/v2/"+name+path, nil, header)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read only

	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("%w: %s%s: not found", ErrRegistry, repository, path)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%w: %s%s: %s", ErrRegistry, repository, path, resp.Status)
	}

	return data, resp.Header, nil
}

// Tags lists the tags of a repository.
func (c *Client) Tags(ctx context.Context, repository string) ([]string, error) {
	data, _, err := c.get(ctx, repository, "/tags/list", "", "pull")
	if err != nil {
		return nil, err
	}

	var list struct {
		Tags []string `json:"tags"`
	}

	return list.Tags, json.Unmarshal(data, &list)
}

// Resolve reads the manifest of a tag: the module it holds and the digest
// of the manifest.
func (c *Client) Resolve(ctx context.Context, repository, tag string) (Manifest, string, error) {
	data, _, err := c.get(ctx, repository, "/manifests/"+url.PathEscape(tag), manifestType, "pull")
	if err != nil {
		return Manifest{}, "", err
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, "", err
	}

	if m.ArtifactType != ArtifactType || len(m.Layers) != 1 || m.Layers[0].MediaType != LayerType {
		return Manifest{}, "", fmt.Errorf("%w: %s:%s is not a module of Rigger", ErrRegistry, repository, tag)
	}

	return m, digestOf(data), nil
}

// Blob reads a blob and checks its digest.
func (c *Client) Blob(ctx context.Context, repository, digest string) ([]byte, error) {
	data, _, err := c.get(ctx, repository, "/blobs/"+digest, "", "pull")
	if err != nil {
		return nil, err
	}

	if digestOf(data) != digest {
		return nil, fmt.Errorf("%w: blob %s has another digest", ErrRegistry, digest)
	}

	return data, nil
}

// ModuleOf reads what a manifest says of its module.
func ModuleOf(m Manifest) Module {
	a := m.Annotations

	return Module{Name: a[annotationModule], Version: a[annotationVer], Path: a[annotationPath], Digest: a[annotationDigest]}
}

// Push pushes a module version: its layer, the empty config and the
// manifest, tagged with the version. It returns the digest of the manifest.
func (c *Client) Push(ctx context.Context, repository string, mod Module, layer []byte) (string, error) {
	if err := c.pushBlob(ctx, repository, []byte(emptyJSON)); err != nil {
		return "", err
	}

	if err := c.pushBlob(ctx, repository, layer); err != nil {
		return "", err
	}

	m := Manifest{
		SchemaVersion: 2, MediaType: manifestType, ArtifactType: ArtifactType,
		Config: Descriptor{MediaType: emptyType, Digest: digestOf([]byte(emptyJSON)), Size: int64(len(emptyJSON))},
		Layers: []Descriptor{{MediaType: LayerType, Digest: digestOf(layer), Size: int64(len(layer))}},
		Annotations: map[string]string{
			annotationModule: mod.Name, annotationVer: mod.Version, annotationPath: mod.Path, annotationDigest: mod.Digest,
		},
	}

	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}

	host, name, err := ref(repository)
	if err != nil {
		return "", err
	}

	resp, err := c.do(ctx, http.MethodPut, host, "repository:"+name+":pull,push",
		baseURL(host)+"/v2/"+name+"/manifests/"+url.PathEscape(mod.Version), data, http.Header{"Content-Type": {manifestType}})
	if err != nil {
		return "", err
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: manifest: %s", ErrRegistry, resp.Status)
	}

	return digestOf(data), nil
}

// pushBlob uploads a blob in one request, unless the registry has it.
func (c *Client) pushBlob(ctx context.Context, repository string, data []byte) error {
	host, name, err := ref(repository)
	if err != nil {
		return err
	}

	scope := "repository:" + name + ":pull,push"
	digest := digestOf(data)
	base := baseURL(host) + "/v2/" + name

	head, err := c.do(ctx, http.MethodHead, host, scope, base+"/blobs/"+digest, nil, nil)
	if err != nil {
		return err
	}

	_ = head.Body.Close()

	if head.StatusCode == http.StatusOK {
		return nil
	}

	start, err := c.do(ctx, http.MethodPost, host, scope, base+"/blobs/uploads/", nil, nil)
	if err != nil {
		return err
	}

	_ = start.Body.Close()

	if start.StatusCode != http.StatusAccepted {
		return fmt.Errorf("%w: upload: %s", ErrRegistry, start.Status)
	}

	loc, err := url.Parse(start.Header.Get("Location"))
	if err != nil {
		return err
	}

	target := (&url.URL{Scheme: "x", Host: "x"}).ResolveReference(loc)
	if loc.Host == "" {
		b, _ := url.Parse(baseURL(host))
		target = b.ResolveReference(loc)
	}

	q := target.Query()
	q.Set("digest", digest)
	target.RawQuery = q.Encode()

	put, err := c.do(ctx, http.MethodPut, host, scope, target.String(), data, http.Header{"Content-Type": {"application/octet-stream"}})
	if err != nil {
		return err
	}

	_ = put.Body.Close()

	if put.StatusCode != http.StatusCreated {
		return fmt.Errorf("%w: blob: %s", ErrRegistry, put.Status)
	}

	return nil
}

// digestOf is the OCI digest of data.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// Layer writes the layer of a module: the files of the directories of root
// in a tar.gz, sorted, with no time, owner or mode of the machine.
func Layer(root fs.FS, dirs []string) ([]byte, error) {
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	var files []string

	for _, dir := range dirs {
		err := fs.WalkDir(root, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if !d.IsDir() {
				files = append(files, p)
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	slices.Sort(files)

	for _, p := range slices.Compact(files) {
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return nil, err
		}

		h := &tar.Header{Name: p, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}

		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
