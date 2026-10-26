// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func runSBOM(args []string) error {
	fs := flag.NewFlagSet("sbom", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	bin := fs.String("binary", "", "the built binary")
	name := fs.String("name", "", "the name of the binary (default: the file name without .exe)")
	out := fs.String("out", "", "write the SBOM to this file (default: standard output)")
	source := fs.String("source-url", "", "URL of the source code of this build (e.g. the release tag), recorded as a VCS reference")
	_ = fs.Parse(args)
	if *bin == "" {
		return errors.New("sbom: -binary is required")
	}
	if *name == "" {
		*name = strings.TrimSuffix(filepath.Base(*bin), ".exe")
	}
	doc, err := buildSBOM(c, *bin, *name, *source)
	if err != nil {
		return err
	}
	if *out == "" {
		_, err = os.Stdout.Write(doc)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, doc, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	return nil
}

// CycloneDX 1.5 JSON (https://cyclonedx.org/docs/1.5/json/), the subset
// used here.
type cdxBOM struct {
	BOMFormat    string          `json:"bomFormat"`
	SpecVersion  string          `json:"specVersion"`
	SerialNumber string          `json:"serialNumber"`
	Version      int             `json:"version"`
	Metadata     cdxMetadata     `json:"metadata"`
	Components   []cdxComponent  `json:"components"`
	Dependencies []cdxDependency `json:"dependencies"`
}

type cdxMetadata struct {
	Timestamp string       `json:"timestamp,omitempty"`
	Tools     cdxTools     `json:"tools"`
	Component cdxComponent `json:"component"`
}

type cdxTools struct {
	Components []cdxComponent `json:"components"`
}

type cdxComponent struct {
	BOMRef             string        `json:"bom-ref,omitempty"`
	Type               string        `json:"type"`
	Group              string        `json:"group,omitempty"`
	Name               string        `json:"name"`
	Version            string        `json:"version,omitempty"`
	Description        string        `json:"description,omitempty"`
	Hashes             []cdxHash     `json:"hashes,omitempty"`
	Licenses           []cdxLicense  `json:"licenses,omitempty"`
	PURL               string        `json:"purl,omitempty"`
	ExternalReferences []cdxExtRef   `json:"externalReferences,omitempty"`
	Properties         []cdxProperty `json:"properties,omitempty"`
}

type cdxHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type cdxLicense struct {
	License    *cdxLicenseID `json:"license,omitempty"`
	Expression string        `json:"expression,omitempty"`
}

type cdxLicenseID struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type cdxExtRef struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type cdxDependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// cdxLicenses returns the CycloneDX licenses of an SPDX expression: a
// single identifier, or an expression.
func cdxLicenses(expr string) []cdxLicense {
	switch {
	case expr == "":
		return nil
	case strings.HasPrefix(expr, "LicenseRef-") && !strings.ContainsAny(expr, " ()"):
		// Not an SPDX list identifier: CycloneDX takes it as a name.
		return []cdxLicense{{License: &cdxLicenseID{Name: expr}}}
	case !strings.ContainsAny(expr, " ()"):
		return []cdxLicense{{License: &cdxLicenseID{ID: expr}}}
	default:
		return []cdxLicense{{Expression: expr}}
	}
}

// purlEscape percent-encodes a purl path segment or version.
func purlEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%2F", "/")
}

var norcaVersionRe = regexp.MustCompile(`(?m)^\s*NorcaVersion\s*=\s*"([^"]+)"`)

func norcaVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "nomad", "version", "version.go"))
	if err != nil {
		return "", err
	}
	m := norcaVersionRe.FindSubmatch(b)
	if m == nil {
		return "", errors.New("NorcaVersion not found in nomad/version/version.go")
	}
	return string(m[1]), nil
}

func buildSBOM(c commonFlags, binPath, name, sourceURL string) ([]byte, error) {
	var b *binary
	for i := range binaries {
		if binaries[i].Name == name {
			b = &binaries[i]
		}
	}
	if b == nil {
		return nil, fmt.Errorf("unknown binary %q", name)
	}
	data, err := os.ReadFile(binPath)
	if err != nil {
		return nil, err
	}
	bi, err := buildinfo.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", binPath, err)
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	var tags []string
	if t := settings["-tags"]; t != "" {
		tags = strings.Split(t, ",")
	}

	// The licenses come from the same module data as the notices: list the
	// packages of the binary's configuration and check that the modules
	// match the build information.
	mods := map[string]*goModule{}
	if err := listDeps(c.root, *b, settings["GOOS"], settings["GOARCH"], settings["CGO_ENABLED"] == "1", tags, b.Package, mods); err != nil {
		return nil, err
	}
	problems, err := resolveGoLicenses(c.root, mods)
	if err != nil {
		return nil, err
	}
	linked := map[string]bool{}
	for _, d := range bi.Deps {
		if d.Replace != nil && (strings.HasPrefix(d.Replace.Path, ".") || strings.HasPrefix(d.Replace.Path, "/") || filepath.IsAbs(d.Replace.Path)) {
			continue // replaced by a local directory: Norca's own api and controller modules
		}
		linked[d.Path+"@"+d.Version] = true
		if mods[d.Path+"@"+d.Version] == nil {
			problems = append(problems, fmt.Sprintf("%s@%s is linked into %s but not found by go list (is the binary built from this tree?)", d.Path, d.Version, binPath))
		} else {
			mods[d.Path+"@"+d.Version].Sum = d.Sum
			if d.Replace != nil {
				mods[d.Path+"@"+d.Version].Sum = d.Replace.Sum
			}
		}
	}
	for k := range mods {
		if k != stdlib && !linked[k] && mods[k].CopyDir == "" {
			problems = append(problems, fmt.Sprintf("%s is found by go list but not linked into %s (is the binary built from this tree?)", k, binPath))
		}
	}

	var npm []*npmPackage
	if b.UI && !c.noUI && contains(tags, "ui") {
		var p []string
		var err error
		if npm, p, err = collectUIs(c); err != nil {
			return nil, err
		}
		problems = append(problems, p...)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, licenseError(problems)
	}
	if err := checkOverrides(c.strict, false, c.kinds()...); err != nil {
		return nil, err
	}

	version, err := norcaVersion(c.root)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	mainRef := "norca:" + name + "@" + version
	main := cdxComponent{
		BOMRef:      mainRef,
		Type:        "application",
		Name:        name,
		Version:     version,
		Description: fmt.Sprintf("%s %s for %s/%s", name, version, settings["GOOS"], settings["GOARCH"]),
		Hashes:      []cdxHash{{Alg: "SHA-256", Content: hex.EncodeToString(sum[:])}},
		Licenses:    cdxLicenses("MPL-2.0"),
	}
	if sourceURL != "" {
		main.ExternalReferences = []cdxExtRef{{Type: "vcs", URL: sourceURL}}
	}
	props := []cdxProperty{
		{"norca:go:module", bi.Main.Path},
		{"norca:go:mainPackage", bi.Path},
		{"norca:go:toolchain", bi.GoVersion},
	}
	for _, k := range []string{"GOOS", "GOARCH", "CGO_ENABLED", "-tags", "vcs.revision", "vcs.time", "vcs.modified"} {
		if v, ok := settings[k]; ok {
			props = append(props, cdxProperty{"norca:build:" + strings.TrimPrefix(k, "-"), v})
		}
	}
	main.Properties = props

	var comps []cdxComponent
	var refs []string
	for _, k := range sortedModKeys(mods) {
		m := mods[k]
		var comp cdxComponent
		if m.Path == stdlib {
			v := strings.TrimPrefix(bi.GoVersion, "go")
			comp = cdxComponent{
				Type:     "library",
				Name:     "stdlib",
				Version:  v,
				Licenses: cdxLicenses(m.Expr),
				PURL:     "pkg:golang/stdlib@v" + purlEscape(v),
				Properties: []cdxProperty{
					{"norca:go:description", "Go standard library"},
				},
			}
		} else {
			path, ver := m.Path, m.Version
			if m.ReplacePath != "" {
				path, ver = m.ReplacePath, m.ReplaceVersion
			}
			comp = cdxComponent{
				Type:     "library",
				Name:     path,
				Version:  ver,
				Licenses: cdxLicenses(m.Expr),
				PURL:     "pkg:golang/" + purlEscape(path) + "@" + purlEscape(ver),
			}
			if m.ReplacePath != "" {
				comp.Properties = append(comp.Properties, cdxProperty{"norca:go:replaces", m.Path + "@" + m.Version})
			}
			if m.CopyDir != "" {
				comp.Properties = append(comp.Properties, cdxProperty{"norca:source:copy", m.CopyDir})
			}
			if m.Sum != "" {
				comp.Properties = append(comp.Properties, cdxProperty{"norca:go:sum", m.Sum})
			}
		}
		comp.BOMRef = comp.PURL
		comps = append(comps, comp)
		refs = append(refs, comp.BOMRef)
	}
	for _, p := range npm {
		group, pname := "", p.Name
		if strings.HasPrefix(p.Name, "@") {
			if i := strings.IndexByte(p.Name, '/'); i > 0 {
				group, pname = p.Name[:i], p.Name[i+1:]
			}
		}
		purl := "pkg:npm/"
		if group != "" {
			// The purl spec requires the scope's "@" to be encoded.
			purl += strings.ReplaceAll(url.PathEscape(group), "@", "%40") + "/"
		}
		purl += url.PathEscape(pname) + "@" + url.PathEscape(p.Version)
		comp := cdxComponent{
			BOMRef:   purl,
			Type:     "library",
			Group:    group,
			Name:     pname,
			Version:  p.Version,
			Licenses: cdxLicenses(p.Expr),
			PURL:     purl,
			Properties: []cdxProperty{
				{"norca:npm:usage", "web UI embedded in " + name},
			},
		}
		if p.Resolved != "" {
			u := p.Resolved
			if i := strings.IndexByte(u, '#'); i >= 0 {
				u = u[:i]
			}
			comp.ExternalReferences = []cdxExtRef{{Type: "distribution", URL: u}}
		}
		comps = append(comps, comp)
		refs = append(refs, purl)
	}
	deps := []cdxDependency{{Ref: mainRef, DependsOn: refs}}
	for _, r := range refs {
		deps = append(deps, cdxDependency{Ref: r, DependsOn: []string{}})
	}

	bom := cdxBOM{
		BOMFormat:    "CycloneDX",
		SpecVersion:  "1.5",
		SerialNumber: serialNumber(sum[:]),
		Version:      1,
		Metadata: cdxMetadata{
			Timestamp: settings["vcs.time"],
			Tools: cdxTools{Components: []cdxComponent{{
				Type:        "application",
				Group:       "norca",
				Name:        "thirdparty",
				Description: "nomad/tools/thirdparty",
			}}},
			Component: main,
		},
		Components:   comps,
		Dependencies: deps,
	}
	out, err := json.MarshalIndent(bom, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// serialNumber derives a name-based (version 5) UUID URN from the binary's
// hash, so that the same binary always gets the same serial number.
func serialNumber(binSum []byte) string {
	h := sha1.New()
	h.Write([]byte("norca-sbom:"))
	h.Write(binSum)
	u := h.Sum(nil)[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	x := hex.EncodeToString(u)
	return fmt.Sprintf("urn:uuid:%s-%s-%s-%s-%s", x[0:8], x[8:12], x[12:16], x[16:20], x[20:32])
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
