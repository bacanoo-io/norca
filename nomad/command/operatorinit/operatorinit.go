// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package operatorinit scaffolds an operator project for the Norca
// controller SDK ("norca operator init"): a Go module with the controller
// (reconciler with a finalizer, status conditions, event log, leader
// election, metrics), a validating webhook, tests against the SDK's fake
// backend and against a dev agent (envtest), the object kind with its JSON
// Schema and a sample object, and an operator pack that installs the kind,
// the ACL policy of the controller and the controller job as one release.
package operatorinit

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"unicode"
)

// templates holds the project files; names starting with "_" (pack helpers,
// _.gitignore) need the all: prefix.
//
//go:embed all:templates
var templates embed.FS

// Options describe the project to scaffold.
type Options struct {
	// Dir is the directory of the project. It is created; existing files
	// are only overwritten with Force.
	Dir string

	// Group and Kind name the object kind ("db.example.com",
	// "PostgresCluster").
	Group string
	Kind  string

	// Name of the operator (default: "<kind in lower case>-operator"). It
	// names the binary, the controller, the job and the pack.
	Name string

	// Module is the Go module path (default: "example.com/<name>").
	Module string

	// ClusterScoped makes the kind cluster-scoped instead of namespaced.
	ClusterScoped bool

	// SDKPath is the "nomad" directory of a Norca checkout. If set, go.mod
	// replaces the SDK and API modules with its controller and api
	// directories; otherwise the replace directives are left commented.
	SDKPath string

	// Force overwrites existing files.
	Force bool
}

var (
	validGroup = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	validKind  = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)
	validName  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// data is what the templates see.
type data struct {
	Group         string
	Kind          string
	KindLower     string
	Name          string
	Module        string
	Namespaced    bool
	Namespace     string // the namespace of the samples and tests ("" for cluster-scoped kinds)
	Finalizer     string
	Schema        string
	SDKPath       string
	ControllerDir string
	APIDir        string
}

// File is a generated file.
type File struct {
	Path    string
	Content []byte
}

// Render returns the files of the project, without writing them.
func Render(opts Options) ([]*File, error) {
	if !validGroup.MatchString(opts.Group) {
		return nil, fmt.Errorf("invalid group %q: use a lower case DNS name with at least one dot, such as db.example.com", opts.Group)
	}
	if !validKind.MatchString(opts.Kind) {
		return nil, fmt.Errorf("invalid kind %q: use a CamelCase name starting with an upper case letter, such as PostgresCluster", opts.Kind)
	}
	d := &data{
		Group:      opts.Group,
		Kind:       opts.Kind,
		KindLower:  strings.ToLower(opts.Kind),
		Name:       opts.Name,
		Module:     opts.Module,
		Namespaced: !opts.ClusterScoped,
	}
	if d.Name == "" {
		d.Name = kebab(opts.Kind) + "-operator"
	}
	if !validName.MatchString(d.Name) {
		return nil, fmt.Errorf("invalid operator name %q: use lower case letters, digits and dashes", d.Name)
	}
	if d.Module == "" {
		d.Module = "example.com/" + d.Name
	}
	if d.Namespaced {
		d.Namespace = "default"
	}
	d.Finalizer = opts.Group + "/" + kebab(opts.Kind) + "-cleanup"
	if opts.SDKPath != "" {
		abs, err := filepath.Abs(opts.SDKPath)
		if err != nil {
			return nil, err
		}
		d.SDKPath = filepath.ToSlash(abs)
		d.ControllerDir = d.SDKPath + "/controller"
		d.APIDir = d.SDKPath + "/api"
	}
	schema, err := templates.ReadFile("templates/schema.json")
	if err != nil {
		return nil, err
	}
	d.Schema = strings.TrimSpace(string(schema))

	var files []*File
	err = fs.WalkDir(templates, "templates", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return err
		}
		src, err := templates.ReadFile(path)
		if err != nil {
			return err
		}
		tmpl, err := template.New(path).Delims("<%", "%>").Parse(string(src))
		if err != nil {
			return fmt.Errorf("template %s: %w", path, err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, d); err != nil {
			return fmt.Errorf("template %s: %w", path, err)
		}
		out := strings.TrimSuffix(strings.TrimPrefix(path, "templates/"), ".tmpl")
		out = strings.ReplaceAll(out, "KIND", d.KindLower)
		out = strings.ReplaceAll(out, "NAME", d.Name)
		out = strings.ReplaceAll(out, "_.", ".") // _.gitignore -> .gitignore
		content := bytes.TrimLeft(buf.Bytes(), "\n")
		files = append(files, &File{Path: out, Content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// Generate renders the project and writes it to opts.Dir. It returns the
// paths written, relative to the directory.
func Generate(opts Options) ([]string, error) {
	if opts.Dir == "" {
		return nil, errors.New("missing project directory")
	}
	files, err := Render(opts)
	if err != nil {
		return nil, err
	}
	if !opts.Force {
		var existing []string
		for _, f := range files {
			if _, err := os.Stat(filepath.Join(opts.Dir, filepath.FromSlash(f.Path))); err == nil {
				existing = append(existing, f.Path)
			}
		}
		if len(existing) > 0 {
			return nil, fmt.Errorf("files exist (use -force to overwrite): %s", strings.Join(existing, ", "))
		}
	}
	var written []string
	for _, f := range files {
		path := filepath.Join(opts.Dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(path, f.Content, 0o644); err != nil {
			return written, err
		}
		written = append(written, f.Path)
	}
	return written, nil
}

// kebab converts a CamelCase name to lower case words joined by dashes:
// PostgresCluster -> postgres-cluster.
func kebab(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
				b.WriteByte('-')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
