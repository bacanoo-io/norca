// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// lockEntry is an entry of a yarn v1 lockfile.
type lockEntry struct {
	Version      string
	Resolved     string
	Integrity    string
	Dependencies map[string]string
}

// parseYarnLock parses a yarn v1 lockfile into a map from "name@range" to
// its entry.
func parseYarnLock(data []byte) (map[string]*lockEntry, error) {
	out := map[string]*lockEntry{}
	var cur *lockEntry
	inDeps := false
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		l := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "#") {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		t := strings.TrimSpace(l)
		switch indent {
		case 0:
			if !strings.HasSuffix(t, ":") {
				return nil, fmt.Errorf("yarn.lock:%d: unexpected line %q", line, t)
			}
			cur = &lockEntry{Dependencies: map[string]string{}}
			inDeps = false
			for _, k := range strings.Split(strings.TrimSuffix(t, ":"), ",") {
				out[unquote(strings.TrimSpace(k))] = cur
			}
		case 2:
			if cur == nil {
				return nil, fmt.Errorf("yarn.lock:%d: field outside an entry", line)
			}
			inDeps = false
			if t == "dependencies:" || t == "optionalDependencies:" {
				inDeps = true
				continue
			}
			k, v := splitField(t)
			switch k {
			case "version":
				cur.Version = v
			case "resolved":
				cur.Resolved = v
			case "integrity":
				cur.Integrity = v
			}
		case 4:
			if cur == nil || !inDeps {
				return nil, fmt.Errorf("yarn.lock:%d: unexpected dependency line", line)
			}
			k, v := splitField(t)
			cur.Dependencies[k] = v
		default:
			return nil, fmt.Errorf("yarn.lock:%d: unexpected indentation", line)
		}
	}
	return out, sc.Err()
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// splitField splits `key "value"` (either side may be quoted).
func splitField(t string) (string, string) {
	var k string
	if strings.HasPrefix(t, `"`) {
		end := strings.Index(t[1:], `"`)
		if end < 0 {
			return t, ""
		}
		k, t = t[1:end+1], t[end+2:]
	} else {
		i := strings.IndexByte(t, ' ')
		if i < 0 {
			return t, ""
		}
		k, t = t[:i], t[i:]
	}
	return k, unquote(strings.TrimSpace(t))
}

// npmPackage is an npm package bundled into the web UI.
type npmPackage struct {
	Name    string
	Version string
	// Declared is the license declared in package.json.
	Declared string
	Author   string
	Homepage string
	Resolved string
	// Via is the dependency path from nomad/ui/package.json.
	Via string

	Licenses []textFile
	Notices  []textFile
	// Expr is the license expression shown for the package.
	Expr     string
	Override *override
}

func (p *npmPackage) key() string { return p.Name + "@" + p.Version }

type packageJSON struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	License  json.RawMessage `json:"license"`
	Licenses json.RawMessage `json:"licenses"`
	Author   json.RawMessage `json:"author"`
	Homepage string          `json:"homepage"`

	Dependencies map[string]string `json:"dependencies"`
}

// declaredLicense returns the license of a package.json as an SPDX
// expression (the legacy "licenses" array is joined with OR).
func (p *packageJSON) declaredLicense() string {
	if s := licenseValue(p.License); s != "" {
		return s
	}
	var list []json.RawMessage
	if json.Unmarshal(p.Licenses, &list) == nil {
		var ids []string
		for _, l := range list {
			if s := licenseValue(l); s != "" {
				ids = append(ids, s)
			}
		}
		if len(ids) == 1 {
			return ids[0]
		}
		if len(ids) > 1 {
			return "(" + strings.Join(ids, " OR ") + ")"
		}
	}
	return ""
}

func licenseValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return strings.TrimSpace(o.Type)
	}
	return ""
}

func (p *packageJSON) author() string {
	var s string
	if json.Unmarshal(p.Author, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if json.Unmarshal(p.Author, &o) == nil && o.Name != "" {
		if o.Email != "" {
			return o.Name + " <" + o.Email + ">"
		}
		return o.Name
	}
	return ""
}

// npmSource provides the files of npm packages.
type npmSource struct {
	nodeModules map[string]string // "name@version" -> directory
	cacheDir    string
	offline     bool
	client      *http.Client
}

// pkgFiles holds the package.json and the top-level license and NOTICE
// files of a package.
type pkgFiles struct {
	packageJSON []byte
	licenses    map[string][]byte
	notices     map[string][]byte
}

func newNPMSource(uiDir, cacheDir string, offline bool) (*npmSource, error) {
	s := &npmSource{
		nodeModules: map[string]string{},
		cacheDir:    cacheDir,
		offline:     offline,
		client:      &http.Client{Timeout: 2 * time.Minute},
	}
	if s.cacheDir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		s.cacheDir = filepath.Join(d, "norca-thirdparty")
	}
	indexNodeModules(filepath.Join(uiDir, "node_modules"), s.nodeModules)
	return s, nil
}

// indexNodeModules records the packages installed in a node_modules
// directory and in the node_modules directories nested in them.
func indexNodeModules(dir string, idx map[string]string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), "@") {
			scoped, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, se := range scoped {
				if se.IsDir() {
					indexPackage(filepath.Join(p, se.Name()), idx)
				}
			}
			continue
		}
		indexPackage(p, idx)
	}
}

func indexPackage(dir string, idx map[string]string) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return
	}
	var pj packageJSON
	if json.Unmarshal(b, &pj) != nil || pj.Name == "" {
		return
	}
	k := pj.Name + "@" + pj.Version
	if _, ok := idx[k]; !ok {
		idx[k] = dir
	}
	indexNodeModules(filepath.Join(dir, "node_modules"), idx)
}

// files returns the files of the package that a lockfile entry resolves to.
func (s *npmSource) files(name string, e *lockEntry) (*pkgFiles, error) {
	if dir, ok := s.nodeModules[name+"@"+e.Version]; ok {
		return readPackageDir(dir)
	}
	tgz, err := s.tarball(name, e)
	if err != nil {
		return nil, err
	}
	return readTarball(tgz)
}

func readPackageDir(dir string) (*pkgFiles, error) {
	pf := &pkgFiles{licenses: map[string][]byte{}, notices: map[string][]byte{}}
	var err error
	if pf.packageJSON, err = os.ReadFile(filepath.Join(dir, "package.json")); err != nil {
		return nil, err
	}
	for _, n := range findFiles(dir, licenseFileRe) {
		if pf.licenses[n], err = os.ReadFile(filepath.Join(dir, n)); err != nil {
			return nil, err
		}
	}
	for _, n := range findFiles(dir, noticeFileRe) {
		if pf.notices[n], err = os.ReadFile(filepath.Join(dir, n)); err != nil {
			return nil, err
		}
	}
	return pf, nil
}

// readTarball reads the package.json and the top-level license and NOTICE
// files of an npm package tarball.
func readTarball(data []byte) (*pkgFiles, error) {
	pf := &pkgFiles{licenses: map[string][]byte{}, notices: map[string][]byte{}}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		// Entries are below a single top-level directory, usually package/.
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		parts := strings.Split(name, "/")
		if len(parts) != 2 {
			continue
		}
		base := parts[1]
		var dst map[string][]byte
		switch {
		case base == "package.json":
		case matchFileName(base, licenseFileRe):
			dst = pf.licenses
		case matchFileName(base, noticeFileRe):
			dst = pf.notices
		default:
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if dst == nil {
			pf.packageJSON = b
		} else {
			dst[base] = b
		}
	}
	if pf.packageJSON == nil {
		return nil, fmt.Errorf("no package.json in the tarball")
	}
	return pf, nil
}

// tarball returns the verified tarball of a package from the cache, or
// downloads it.
func (s *npmSource) tarball(name string, e *lockEntry) ([]byte, error) {
	url := e.Resolved
	frag := ""
	if i := strings.IndexByte(url, '#'); i >= 0 {
		url, frag = url[:i], url[i+1:]
	}
	if !strings.HasPrefix(url, "https://registry.yarnpkg.com/") && !strings.HasPrefix(url, "https://registry.npmjs.org/") {
		return nil, fmt.Errorf("%s@%s: not resolved from the npm registry (%s); install nomad/ui/node_modules", name, e.Version, e.Resolved)
	}
	verify, err := verifier(e.Integrity, frag)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %v", name, e.Version, err)
	}
	sum := sha256.Sum256([]byte(url))
	cached := filepath.Join(s.cacheDir, "npm", hex.EncodeToString(sum[:16])+".tgz")
	if b, err := os.ReadFile(cached); err == nil && verify(b) == nil {
		return b, nil
	}
	if s.offline {
		return nil, fmt.Errorf("%s@%s: not in node_modules or the cache, and -offline is set", name, e.Version)
	}
	resp, err := s.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %v", name, e.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s@%s: GET %s: %s", name, e.Version, url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("%s@%s: %v", name, e.Version, err)
	}
	if err := verify(b); err != nil {
		return nil, fmt.Errorf("%s@%s: %v", name, e.Version, err)
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err == nil {
		tmp := cached + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, cached)
		}
	}
	return b, nil
}

// verifier returns a function that checks data against the lockfile's
// integrity value (Subresource Integrity, sha512 preferred) or, without one,
// the sha1 in the resolved URL's fragment.
func verifier(integrity, frag string) (func([]byte) error, error) {
	type want struct {
		h   func() hash.Hash
		sum []byte
	}
	var ws []want
	for _, f := range strings.Fields(integrity) {
		alg, b64, ok := strings.Cut(f, "-")
		if !ok {
			continue
		}
		sum, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		switch alg {
		case "sha512":
			ws = append(ws, want{sha512.New, sum})
		case "sha1":
			ws = append(ws, want{sha1.New, sum})
		}
	}
	if len(ws) == 0 && frag != "" {
		if sum, err := hex.DecodeString(frag); err == nil {
			ws = append(ws, want{sha1.New, sum})
		}
	}
	if len(ws) == 0 {
		return nil, fmt.Errorf("no integrity hash in yarn.lock")
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].h().Size() > ws[j].h().Size() })
	w := ws[0]
	return func(data []byte) error {
		h := w.h()
		h.Write(data)
		if !bytes.Equal(h.Sum(nil), w.sum) {
			return fmt.Errorf("integrity check failed")
		}
		return nil
	}, nil
}

// collectNPM resolves the production dependencies of the UI (the
// "dependencies" of package.json and, transitively, their dependencies and
// optional dependencies) through yarn.lock and reads their license files.
func collectNPM(uiDir string, src *npmSource) ([]*npmPackage, []string, error) {
	pjData, err := os.ReadFile(filepath.Join(uiDir, "package.json"))
	if err != nil {
		return nil, nil, err
	}
	var pj packageJSON
	if err := json.Unmarshal(pjData, &pj); err != nil {
		return nil, nil, fmt.Errorf("nomad/ui/package.json: %v", err)
	}
	lockData, err := os.ReadFile(filepath.Join(uiDir, "yarn.lock"))
	if err != nil {
		return nil, nil, err
	}
	lock, err := parseYarnLock(lockData)
	if err != nil {
		return nil, nil, err
	}

	type item struct{ name, rng, parent string }
	var queue []item
	for _, n := range sortedStringKeys(pj.Dependencies) {
		queue = append(queue, item{n, pj.Dependencies[n], ""})
	}
	entries := map[string]*lockEntry{} // "name@version" -> entry
	names := map[string]string{}       // "name@version" -> name
	parents := map[string]string{}     // "name@version" -> the first package that requires it
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		e, ok := lock[it.name+"@"+it.rng]
		if !ok {
			return nil, nil, fmt.Errorf("yarn.lock has no entry for %s@%s; run yarn install in nomad/ui", it.name, it.rng)
		}
		k := it.name + "@" + e.Version
		if _, done := entries[k]; done {
			continue
		}
		entries[k] = e
		names[k] = it.name
		parents[k] = it.parent
		for _, d := range sortedStringKeys(e.Dependencies) {
			queue = append(queue, item{d, e.Dependencies[d], k})
		}
	}
	// via returns the dependency path from package.json to a package.
	via := func(k string) string {
		path := []string{k}
		for p := parents[k]; p != ""; p = parents[p] {
			path = append([]string{p}, path...)
		}
		return strings.Join(path, " > ")
	}

	var pkgs []*npmPackage
	var problems []string
	for _, k := range sortedStringKeys(entries) {
		e, name := entries[k], names[k]
		pf, err := src.files(name, e)
		if err != nil {
			return nil, nil, err
		}
		var meta packageJSON
		if err := json.Unmarshal(pf.packageJSON, &meta); err != nil {
			return nil, nil, fmt.Errorf("%s: package.json: %v", k, err)
		}
		p := &npmPackage{
			Name:     name,
			Version:  e.Version,
			Declared: meta.declaredLicense(),
			Author:   meta.author(),
			Homepage: meta.Homepage,
			Resolved: e.Resolved,
			Via:      via(k),
		}
		for _, n := range sortedByteKeys(pf.licenses) {
			p.Licenses = append(p.Licenses, textFile{Path: n, Text: normalizeText(string(pf.licenses[n]))})
		}
		for _, n := range sortedByteKeys(pf.notices) {
			p.Notices = append(p.Notices, textFile{Path: n, Text: normalizeText(string(pf.notices[n]))})
		}
		ids := licenseIDs(p.Licenses)
		p.Expr = p.Declared
		if p.Expr == "" {
			// No declared license: use the license files.
			p.Expr = strings.Join(ids, " AND ")
		}
		pkgs = append(pkgs, p)

		if forbiddenNPM[name] {
			problems = append(problems, fmt.Sprintf("npm %s: the package is forbidden (docs/licensing.md); via %s", k, p.Via))
			continue
		}
		if o := findOverride("npm", name, e.Version); o != nil {
			p.Override = o
			p.Expr = o.License
			applyExcerpt(o, p.Licenses)
			if !o.Pending && !exprAllowed(o.License, allowedNPM) {
				problems = append(problems, fmt.Sprintf("npm %s: override license %q is not on the allow-list", k, o.License))
			}
			continue
		}
		if p.Expr == "" {
			problems = append(problems, fmt.Sprintf("npm %s: no declared or recognized license (license files: %s); via %s", k, filePaths(p.Licenses), p.Via))
			continue
		}
		if !exprAllowed(p.Expr, allowedNPM) {
			problems = append(problems, fmt.Sprintf("npm %s: license %q is not on the allow-list (license files: %s); via %s", k, p.Expr, filePaths(p.Licenses), p.Via))
		}
	}
	return pkgs, problems, nil
}

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedByteKeys(m map[string][]byte) []string { return sortedStringKeys(m) }
