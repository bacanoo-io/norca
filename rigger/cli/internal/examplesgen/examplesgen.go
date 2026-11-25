// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package examplesgen writes a local example project of Rigger ("rigger
// generate examples"): a registry repository of the user's own with two
// modules, a library module and a published stack; a stack of the user's
// own that installs from that registry by its path on disk, with values
// per environment, trust and a module of its own; an index of the shape of
// the global registry over the registry; and the Registry object that
// registers the registry in a cluster. Every file is commented, so that the
// project shows the whole configuration of a registry, a module and a stack.
//
// The generator only writes files. It runs no git, signs nothing and syncs
// nothing: the README of the project has the commands, in their order.
//
// It is a command of the rigger program only (command.Program.Commands):
// generating files is no part of the cluster, so norca does not carry it.
package examplesgen

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"
)

// files holds the project. Names starting with "_." (_.gitattributes) are
// written with a leading dot instead, so that git does not read them as
// files of this repository; they need the all: prefix to be embedded.
//
//go:embed all:files
var files embed.FS

// filesRoot is the directory of the project in files.
const filesRoot = "files"

// DefaultOwner is the owner when Options.Owner is empty.
const DefaultOwner = "example"

// Options describe the project to write.
type Options struct {
	// Dir is the directory of the project. It is created; existing files
	// are only overwritten with Force.
	Dir string

	// Owner names the registry in the stacks (registry "<owner>"), so that
	// a module is <owner>/<name>, and the namespace of the index of the
	// shape of the global registry. Default: DefaultOwner.
	Owner string

	// Force overwrites existing files.
	Force bool
}

// File is a file of the project.
type File struct {
	// Path is relative to the directory of the project, with slashes.
	Path    string
	Content []byte
	Mode    fs.FileMode
}

// data is what the files see.
type data struct {
	// Owner is the owner of the modules and the stacks.
	Owner string
	// RegistryPath is the absolute path of the registry repository, with
	// slashes: the stacks name the registry by it.
	RegistryPath string
}

// The modes of the files and directories written.
const (
	modeFile   fs.FileMode = 0o644
	modeScript fs.FileMode = 0o755
	modeDir    fs.FileMode = 0o750
)

// validOwner is a name of a registry of a stack and of a namespace of the
// global registry.
var validOwner = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrExists is the error of a project that would overwrite files without
// Options.Force.
var ErrExists = errors.New("files exist (use --force to overwrite)")

// RegistryPath returns the absolute path, with slashes, of the registry
// repository of a project in dir: the address the stacks of the project
// name the registry by.
func RegistryPath(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("the path of %s: %w", dir, err)
	}

	return filepath.ToSlash(filepath.Join(abs, "registry")), nil
}

// Render returns the files of the project, without writing them.
func Render(opts Options) ([]*File, error) {
	d, err := newData(opts)
	if err != nil {
		return nil, err
	}

	funcs := template.FuncMap{"hcl": hclString, "json": jsonString}

	var out []*File

	err = fs.WalkDir(files, filesRoot, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		f, err := renderFile(p, d, funcs)
		if err != nil {
			return err
		}

		out = append(out, f)

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(a, b *File) int { return strings.Compare(a.Path, b.Path) })

	return out, nil
}

// newData checks the options and returns what the files see.
func newData(opts Options) (*data, error) {
	owner := opts.Owner
	if owner == "" {
		owner = DefaultOwner
	}

	if !validOwner.MatchString(owner) {
		return nil, fmt.Errorf("invalid owner %q: use lower case letters, digits and dashes, at most 63, such as acme", owner)
	}

	registry, err := RegistryPath(opts.Dir)
	if err != nil {
		return nil, err
	}

	return &data{Owner: owner, RegistryPath: registry}, nil
}

// renderFile renders the file at p of files: its output path has OWNER
// replaced by the owner and a leading "_." by ".".
func renderFile(p string, d *data, funcs template.FuncMap) (*File, error) {
	src, err := files.ReadFile(p)
	if err != nil {
		return nil, err
	}

	// A checkout with CRLF line ends must not give them to the project:
	// the digest of a module is over its file contents.
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))

	tmpl, err := template.New(p).Delims("<%", "%>").Funcs(funcs).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("template %s: %w", p, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("template %s: %w", p, err)
	}

	rel := strings.TrimPrefix(p, filesRoot+"/")
	rel = strings.ReplaceAll(rel, "OWNER", d.Owner)

	dir, name := path.Split(rel)
	if rest, ok := strings.CutPrefix(name, "_."); ok {
		name = "." + rest
	}

	// The scripts of the project run on their own; a file of a module that
	// a job reads with file() stays a plain file.
	mode := modeFile
	if path.Base(dir) == "scripts" && strings.HasSuffix(name, ".sh") {
		mode = modeScript
	}

	return &File{Path: dir + name, Content: buf.Bytes(), Mode: mode}, nil
}

// Generate renders the project and writes it to opts.Dir. It returns the
// paths written, relative to the directory, also those written before an
// error. Nothing is written outside the directory: the files are written
// through an os.Root of it.
func Generate(opts Options) (written []string, err error) {
	if opts.Dir == "" {
		return nil, errors.New("missing project directory")
	}

	project, err := Render(opts)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.Dir, modeDir); err != nil {
		return nil, err
	}

	root, err := os.OpenRoot(opts.Dir)
	if err != nil {
		return nil, err
	}

	defer func() { err = errors.Join(err, root.Close()) }()

	if !opts.Force {
		if existing := existingFiles(root, project); len(existing) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrExists, strings.Join(existing, ", "))
		}
	}

	written = make([]string, 0, len(project))

	for _, f := range project {
		if err := writeFile(root, f); err != nil {
			return written, err
		}

		written = append(written, f.Path)
	}

	return written, nil
}

// existingFiles are the files of the project that exist in root.
func existingFiles(root *os.Root, project []*File) []string {
	var existing []string

	for _, f := range project {
		if _, err := root.Lstat(filepath.FromSlash(f.Path)); err == nil {
			existing = append(existing, f.Path)
		}
	}

	return existing
}

// writeFile writes one file of the project into root, with its mode also
// when it existed with another.
func writeFile(root *os.Root, f *File) error {
	name := filepath.FromSlash(f.Path)

	if err := root.MkdirAll(filepath.Dir(name), modeDir); err != nil {
		return err
	}

	if err := root.WriteFile(name, f.Content, f.Mode); err != nil {
		return err
	}

	return root.Chmod(name, f.Mode)
}

// hclReplacer escapes a string for a quoted string of HCL: the escapes and
// the starts of an interpolation and of a directive.
var hclReplacer = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "${", "$${", "%{", "%%{", "\n", `\n`, "\r", `\r`, "\t", `\t`)

// hclString is s as a quoted string of HCL.
func hclString(s string) string {
	return `"` + hclReplacer.Replace(s) + `"`
}

// jsonString is s as a string of JSON.
func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)

	return string(b), err
}
