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
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca/rigger/internal/readlimit"
	"github.com/bacanoo-io/norca/rigger/internal/sameorigin"
	"github.com/bacanoo-io/norca/rigger/internal/targz"
	"github.com/bacanoo-io/norca/rigger/internal/userinfo"
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

// CredentialsFunc returns the credentials of a registry, by its host; ok
// is false for none.
type CredentialsFunc func(host string) (user, password string, ok bool, err error)

// Client talks to OCI registries.
type Client struct {
	http *http.Client

	credentials CredentialsFunc

	mu     sync.Mutex
	tokens map[string]string // by registry and scope
}

// NewClient returns an anonymous client; SetCredentials gives it the
// credentials of registries. Those of a registry go to it when it asks, and
// to the token service it names over https, not to where an upload or a
// redirect leads. A registry that is not on this machine does not lead the
// client to it (checkRedirect, tokenService, do).
func NewClient() *Client {
	return &Client{http: &http.Client{CheckRedirect: checkRedirect}, tokens: map[string]string{}}
}

// errLocal is the error of a registry elsewhere that leads the client to
// this machine. A service there answers this machine and no one else: its
// answer to the token request would go back to the registry as the token.
var errLocal = errors.New("a registry elsewhere leads to this machine")

// checkRedirect is sameorigin.CheckRedirect, and refuses a redirect to this
// machine of a request that did not start there.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && mayBeLocal(req.URL.Hostname()) && !mayBeLocal(via[0].URL.Hostname()) {
		return fmt.Errorf("%w: %w: a redirect of %s to %s", ErrRegistry, errLocal, via[0].URL.Host, req.URL.Host)
	}

	return sameorigin.CheckRedirect(req, via)
}

// SetCredentials sets where the credentials of a registry come from: asked
// by its host, so that a registry gets its own and no other's.
func (c *Client) SetCredentials(fn CredentialsFunc) { c.credentials = fn }

// maxTokenBytes bounds the answer of a token service.
const maxTokenBytes = 1 << 20

// maxResponseBytes bounds a response of a registry: a manifest or a blob.
const maxResponseBytes = 256 << 20

// ErrRegistry is the error of an answer of a registry.
var ErrRegistry = errors.New("OCI registry")

// ErrDigest is the error of what should be a digest and is none.
var ErrDigest = errors.New("not a sha256 digest")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ValidDigest reports whether s is a digest of a manifest or a blob. One
// names a directory of a cache and a path of a registry, so nothing else
// may pass for one.
func ValidDigest(s string) bool {
	return digestPattern.MatchString(s)
}

// ref splits host/name.
func ref(repository string) (host, name string, err error) {
	host, name, ok := strings.Cut(repository, "/")
	if !ok {
		return "", "", fmt.Errorf("%w: %q is not <host>/<repository>", ErrRegistry, userinfo.Redact(repository))
	}

	// A host and nothing more: 127.0.0.1:1@example.com would read as
	// localhost, so http, and be example.com. What is before an at sign may
	// be a password: the error does not repeat it.
	if u, err := url.Parse("https://" + host); err != nil || host == "" || u.Host != host {
		return "", "", fmt.Errorf("%w: %q does not start with a host", ErrRegistry, userinfo.Redact(repository))
	}

	return host, name, nil
}

// baseURL is the URL of a registry: http for localhost, as docker does,
// https otherwise.
func baseURL(host string) string {
	if isLocalhost(hostname(host)) {
		return "http://" + host
	}

	return "https://" + host
}

// hostname is the name or the address of a host without its port and the
// brackets of IPv6.
func hostname(host string) string {
	if u, err := url.Parse("//" + host); err == nil {
		return u.Hostname()
	}

	return host
}

// isLocalhost reports whether a name or an address is this machine as a
// registry is named: localhost, or a loopback address.
func isLocalhost(name string) bool {
	if strings.EqualFold(name, "localhost") {
		return true
	}

	ip := net.ParseIP(name)

	return ip != nil && ip.IsLoopback()
}

// mayBeLocal reports whether a name or an address a registry gave may be
// this machine. It is wider than isLocalhost, for what is refused by it:
// a name of the localhost domain, the address of no host (0.0.0.0), and an
// address in a form net.ParseIP does not read and a resolver may (127.1,
// 2130706433, 0x7f.0.0.1; a name of the DNS does not end in a number).
func mayBeLocal(name string) bool {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}

	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}

	last := name[strings.LastIndex(name, ".")+1:]

	return last != "" && (strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "")
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

	own, err := ofRegistry(host, target)
	if err != nil {
		return nil, err
	}

	// An upload may lead to another origin: it gets no credentials, and
	// can't ask for them. It does not lead to this machine.
	if !own {
		if u, err := url.Parse(target); err == nil && mayBeLocal(u.Hostname()) && !mayBeLocal(hostname(host)) {
			return nil, fmt.Errorf("%w: %w: %s sends an upload to %s", ErrRegistry, errLocal, host, u.Host)
		}

		return send("")
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

	auth, err = c.authorize(ctx, host, challenge, scope)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tokens[key] = auth
	c.mu.Unlock()

	return send(auth)
}

// authorize answers a challenge of the registry host: Basic with its
// credentials, or Bearer with a token of the realm it names.
func (c *Client) authorize(ctx context.Context, host, challenge, scope string) (string, error) {
	var (
		user, password string
		ok             bool
	)

	if c.credentials != nil {
		var err error
		if user, password, ok, err = c.credentials(host); err != nil {
			return "", fmt.Errorf("%w: the credentials of %s: %w", ErrRegistry, host, err)
		}
	}

	if !ok {
		user, password = "", ""
	}

	scheme, params, _ := strings.Cut(challenge, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		if !ok {
			return "", fmt.Errorf("%w: %s asks for credentials, and has none (rigger login %s)", ErrRegistry, host, host)
		}

		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password)), nil
	case "bearer":
		return c.token(ctx, host, parseChallenge(params), scope, user, password)
	default:
		return "", fmt.Errorf("%w: unknown challenge %q", ErrRegistry, challenge)
	}
}

// tokenService is the URL of the token service of a challenge of the
// registry host: https, or http on localhost; and on this machine only
// when the registry is.
func tokenService(host, address string) (*url.URL, error) {
	realm, err := url.Parse(address)
	if err != nil || address == "" {
		return nil, fmt.Errorf("%w: a bearer challenge without a realm", ErrRegistry)
	}

	if realm.Scheme != "https" && (realm.Scheme != "http" || !isLocalhost(realm.Hostname())) {
		return nil, fmt.Errorf("%w: the token service %s://%s is not https (http is for localhost only)", ErrRegistry, realm.Scheme, realm.Host)
	}

	if mayBeLocal(realm.Hostname()) && !mayBeLocal(hostname(host)) {
		return nil, fmt.Errorf("%w: %w: %s names the token service %s://%s", ErrRegistry, errLocal, host, realm.Scheme, realm.Host)
	}

	return realm, nil
}

// ofRegistry reports whether a URL is of the origin of a registry.
func ofRegistry(host, target string) (bool, error) {
	base, err := url.Parse(baseURL(host))
	if err != nil {
		return false, err
	}

	u, err := url.Parse(target)
	if err != nil {
		return false, err
	}

	return sameorigin.Of(u) == sameorigin.Of(base), nil
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

// token gets a bearer token of the realm that the registry host names.
func (c *Client) token(ctx context.Context, host string, params map[string]string, scope, user, password string) (string, error) {
	realm, err := tokenService(host, params["realm"])
	if err != nil {
		return "", err
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

	if user != "" || password != "" {
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

	data, err := readlimit.ReadAll(resp.Body, maxTokenBytes)
	if err != nil {
		return "", fmt.Errorf("%w: token: %w", ErrRegistry, err)
	}

	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.Unmarshal(data, &t); err != nil {
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

	data, err := readlimit.ReadAll(resp.Body, maxResponseBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s%s: %w", ErrRegistry, repository, path, err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, fmt.Errorf("%w: %s%s: not found", ErrRegistry, repository, path)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("%w: %s%s: %s", ErrRegistry, repository, path, resp.Status)
	}

	return data, resp.Header, nil
}

// A registry gives a list of tags in pages. What bounds them: a registry
// that links without an end gets an error, never a list cut short (a
// version that is missing would resolve a constraint to an older one).
const (
	maxTagPages = 1000
	maxTags     = 100_000
	tagsList    = "/tags/list"
)

// Tags lists the tags of a repository: every page of the list, following
// the Link header of each to the next (the distribution API).
func (c *Client) Tags(ctx context.Context, repository string) ([]string, error) {
	var tags []string

	query := ""

	for range maxTagPages {
		data, header, err := c.get(ctx, repository, tagsList+query, "", "pull")
		if err != nil {
			return nil, err
		}

		var list struct {
			Tags []string `json:"tags"`
		}

		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("%w: %s: the list of tags: %w", ErrRegistry, repository, err)
		}

		if tags = append(tags, list.Tags...); len(tags) > maxTags {
			return nil, fmt.Errorf("%w: %s has more than %d tags", ErrRegistry, repository, maxTags)
		}

		next, err := nextTags(repository, strings.Join(header.Values("Link"), ", "))
		if err != nil {
			return nil, err
		}

		switch next {
		case "":
			return tags, nil
		case query:
			return nil, fmt.Errorf("%w: %s: a page of tags links to itself", ErrRegistry, repository)
		}

		query = next
	}

	return nil, fmt.Errorf("%w: %s lists its tags in more than %d pages", ErrRegistry, repository, maxTagPages)
}

// nextTags returns the query of the next page of the tags of a repository
// that a Link header names, with its question mark; "" for the last page.
// The next page is the same list of the same registry: a link to anything
// else is an error, and is not followed.
func nextTags(repository, link string) (string, error) {
	target := nextLink(link)
	if target == "" {
		return "", nil
	}

	host, name, err := ref(repository)
	if err != nil {
		return "", err
	}

	list, err := url.Parse(baseURL(host) + "/v2/" + name + tagsList)
	if err != nil {
		return "", err
	}

	next, err := list.Parse(target)
	if err != nil || sameorigin.Of(next) != sameorigin.Of(list) || next.Path != list.Path || next.RawQuery == "" {
		return "", fmt.Errorf("%w: %s: the next page of tags is not of its list: %q", ErrRegistry, repository, target)
	}

	return "?" + next.RawQuery, nil
}

// nextLink returns the target of the link of the relation next in a Link
// header (RFC 8288), "" for none.
func nextLink(header string) string {
	for header != "" {
		_, rest, ok := strings.Cut(header, "<")
		if !ok {
			break
		}

		target, params, ok := strings.Cut(rest, ">")
		if !ok {
			break
		}

		// The parameters of a link end at the next one.
		if i := strings.Index(params, "<"); i >= 0 {
			params, header = params[:i], params[i:]
		} else {
			header = ""
		}

		for p := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "rel") &&
				slices.Contains(strings.Fields(strings.ToLower(strings.Trim(strings.TrimRight(strings.TrimSpace(v), ","), `"`))), "next") {
				return target
			}
		}
	}

	return ""
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
	if !ValidDigest(digest) {
		return nil, fmt.Errorf("%w: blob %q: %w", ErrRegistry, digest, ErrDigest)
	}

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

	if err := targz.Close(tw, gz, writeLayer(tw, root, dirs)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// writeLayer writes the files of the directories of root into tw, sorted.
func writeLayer(tw *tar.Writer, root fs.FS, dirs []string) error {
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
			return err
		}
	}

	slices.Sort(files)

	for _, p := range slices.Compact(files) {
		data, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}

		h := &tar.Header{Name: p, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}

		if _, err := tw.Write(data); err != nil {
			return err
		}
	}

	return nil
}
