// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package loader

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/nomad/nomad/objectschema"
	"github.com/hashicorp/nomad/pack/internal/pkg/release"
	"github.com/hashicorp/nomad/pack/sdk/pack"
)

func Load(name string) (*pack.Pack, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, errors.New("unable to load non-directory pack")
	}
	return loadDir(name)
}

func loadDir(dir string) (*pack.Pack, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	// Preserve the clean root path before appending the walk separator.
	packRoot := abs

	var files []*pack.File
	abs += string(filepath.Separator)

	walkFn := func(name string, fi os.FileInfo, err error) error {

		if fi.IsDir() {
			return nil
		}

		if err != nil {
			return err
		}

		n := strings.TrimPrefix(name, abs)
		if n == "" {
			return nil
		}

		// Normalize to / since it will also work on Windows
		n = filepath.ToSlash(n)

		if !fi.Mode().IsRegular() {
			return fmt.Errorf("cannot load irregular file %q", name)
		}

		content, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("failed to read %s: %v", n, err)
		}

		content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})

		files = append(files, &pack.File{Name: n, Path: name, Content: content})
		return nil
	}

	if err = walk(abs, walkFn); err != nil {
		return nil, err
	}
	p, err := loadFiles(files)
	if err != nil {
		return p, err
	}
	p.Path = packRoot
	return p, nil
}

func loadFiles(files []*pack.File) (*pack.Pack, error) {

	p := new(pack.Pack)

	// The metadata can read the other files of the pack with file(), so it
	// is decoded once all of them are known.
	var metadataFile *pack.File
	byName := make(map[string]*pack.File, len(files))
	for _, f := range files {
		byName[f.Name] = f
	}

	for _, f := range files {
		switch {
		case f.Name == "metadata.hcl":
			// There shouldn't be more than one per pack, but if there is,
			// last found wins.
			metadataFile = f

		case f.Name == "variables.hcl":
			p.RootVariableFile = f

		case f.Name == "outputs.tpl":
			// This sets the default output template file. It can be overridden
			// from the CLI.
			p.OutputTemplateFile = f

		case strings.HasPrefix(f.Name, "templates/") &&
			(strings.HasSuffix(f.Name, ".nomad.tpl") || release.IsResourceTemplate(f.Name)) ||
			strings.Contains(f.Name, "templates/_"):
			// The file is a pack template file. This catches full Nomad
			// object templates, the other resource templates of releases
			// (e.g. *.namespace.hcl.tpl) and helpers.
			p.TemplateFiles = append(p.TemplateFiles, f)

		case strings.HasPrefix(f.Name, "templates/") &&
			strings.HasSuffix(f.Name, ".tpl"):
			// if there are any other files inside the "templates/" directory,
			// add them to the aux files array
			p.AuxiliaryFiles = append(p.AuxiliaryFiles, f)

		default:
			// Do nothing with other files; this might change in the future.
			continue
		}
	}

	if metadataFile != nil {
		md, err := pack.DecodeMetadata(metadataFile.Name, metadataFile.Content, func(name string) ([]byte, error) {
			f, ok := byName[name]
			if !ok {
				return nil, errors.New("no such file in the pack")
			}
			return f.Content, nil
		})
		if err != nil {
			return p, fmt.Errorf("failed to decode %s: %v", metadataFile.Name, err)
		}
		p.Metadata = md
	}

	// Validate the metadata.
	if p.Metadata == nil {
		return p, errors.New("metadata.hcl file not found")
	}
	if err := p.Metadata.Validate(); err != nil {
		return p, err
	}
	if p.Metadata.VariablesSchema != "" {
		if _, err := objectschema.CompileDocument("pack-variables/"+p.Metadata.Pack.Name, p.Metadata.VariablesSchema); err != nil {
			return p, fmt.Errorf("metadata.hcl: invalid variables_schema: %v", err)
		}
	}
	return p, nil
}
