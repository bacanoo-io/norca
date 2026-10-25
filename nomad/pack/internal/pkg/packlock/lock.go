// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package packlock

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/nomad/pack/sdk/pack"
)

// Version is the format version of the lock file written by this client.
const Version = 1

const header = `# This file is maintained automatically by "norca pack deps vendor" and
# "norca pack ... --lock=update". It records the content digests of the pack and
# of its vendored dependencies; do not edit it by hand.
`

// Lock is the content of a pack.lock file.
type Lock struct {
	Version      int           `hcl:"lock_version"`
	Pack         *PackEntry    `hcl:"pack,block"`
	Dependencies []*Dependency `hcl:"dependency,block"`
}

// PackEntry records the pack itself.
type PackEntry struct {
	Name    string `hcl:"name"`
	Version string `hcl:"version,optional"`

	// Digest is the tree digest of the pack's own content, i.e. without the
	// deps directory and without the lock file.
	Digest string `hcl:"digest"`
}

// Dependency records one vendored dependency of the pack.
type Dependency struct {
	// Name is the dependency's name in metadata.hcl, which is also the name of
	// its directory below deps/.
	Name   string `hcl:"name,label"`
	Source string `hcl:"source,optional"`
	Ref    string `hcl:"ref,optional"`

	// Digest is the tree digest of the vendored dependency directory,
	// including its own vendored dependencies.
	Digest string `hcl:"digest"`

	// ManifestDigest is the OCI manifest digest when the dependency was
	// vendored from an OCI registry.
	ManifestDigest string `hcl:"manifest_digest,optional"`
}

// Path returns the path of the lock file of the pack in dir.
func Path(dir string) string {
	return filepath.Join(dir, FileName)
}

// Read reads the lock file of the pack in dir. It returns os.ErrNotExist
// (wrapped) if the pack has no lock file.
func Read(dir string) (*Lock, error) {
	src, err := os.ReadFile(Path(dir))
	if err != nil {
		return nil, err
	}
	return Parse(src)
}

// Parse decodes the content of a lock file.
func Parse(src []byte) (*Lock, error) {
	// The lock file uses HCL native syntax; the ".hcl" file name selects it.
	file, diags := hclparse.NewParser().ParseHCL(src, FileName+".hcl")
	if diags.HasErrors() {
		return nil, fmt.Errorf("failed to parse %s: %s", FileName, diags.Error())
	}
	lock := &Lock{}
	if diags := gohcl.DecodeBody(file.Body, nil, lock); diags.HasErrors() {
		return nil, fmt.Errorf("failed to decode %s: %s", FileName, diags.Error())
	}
	if lock.Version != Version {
		return nil, fmt.Errorf("unsupported %s version %d", FileName, lock.Version)
	}
	if lock.Pack == nil {
		return nil, fmt.Errorf("%s has no pack block", FileName)
	}
	return lock, nil
}

// Bytes encodes the lock file. The output only depends on the content of the
// lock, so re-generating it for unchanged content produces identical bytes.
func (l *Lock) Bytes() []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.SetAttributeValue("lock_version", cty.NumberIntVal(int64(Version)))
	body.AppendNewline()

	pb := body.AppendNewBlock("pack", nil).Body()
	pb.SetAttributeValue("name", cty.StringVal(l.Pack.Name))
	if l.Pack.Version != "" {
		pb.SetAttributeValue("version", cty.StringVal(l.Pack.Version))
	}
	pb.SetAttributeValue("digest", cty.StringVal(l.Pack.Digest))

	deps := append([]*Dependency(nil), l.Dependencies...)
	sort.Slice(deps, func(i, j int) bool { return deps[i].Name < deps[j].Name })
	for _, d := range deps {
		body.AppendNewline()
		db := body.AppendNewBlock("dependency", []string{d.Name}).Body()
		if d.Source != "" {
			db.SetAttributeValue("source", cty.StringVal(d.Source))
		}
		if d.Ref != "" {
			db.SetAttributeValue("ref", cty.StringVal(d.Ref))
		}
		db.SetAttributeValue("digest", cty.StringVal(d.Digest))
		if d.ManifestDigest != "" {
			db.SetAttributeValue("manifest_digest", cty.StringVal(d.ManifestDigest))
		}
	}

	var buf bytes.Buffer
	buf.WriteString(header)
	buf.WriteString("\n")
	buf.Write(hclwrite.Format(f.Bytes()))
	return buf.Bytes()
}

// Write writes the lock file into the pack directory dir.
func (l *Lock) Write(dir string) error {
	return os.WriteFile(Path(dir), l.Bytes(), 0o644)
}

// Dependency returns the entry of the named dependency, or nil.
func (l *Lock) Dependency(name string) *Dependency {
	for _, d := range l.Dependencies {
		if d.Name == name {
			return d
		}
	}
	return nil
}

// Generate computes the lock of the pack in dir from its current content.
// Every enabled dependency declared in metadata.hcl must be vendored below
// deps/. Source details (manifest digests) of dependencies are taken from
// prev when the dependency's source and ref are unchanged.
func Generate(dir string, metadata *pack.Metadata, prev *Lock) (*Lock, error) {
	if metadata == nil || metadata.Pack == nil {
		return nil, errors.New("pack metadata is required")
	}
	own, err := TreeDigest(dir, OwnContentFilter)
	if err != nil {
		return nil, err
	}
	lock := &Lock{
		Version: Version,
		Pack: &PackEntry{
			Name:    metadata.Pack.Name,
			Version: metadata.Pack.Version,
			Digest:  own,
		},
	}

	for _, dep := range metadata.Dependencies {
		if dep.Enabled != nil && !*dep.Enabled {
			continue
		}
		depDir := filepath.Join(dir, DepsDir, filepath.Clean(dep.Name))
		digest, err := TreeDigest(depDir, ContentFilter)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("dependency %q is not vendored, run \"norca pack deps vendor\"", dep.Name)
			}
			return nil, fmt.Errorf("dependency %q: %w", dep.Name, err)
		}
		entry := &Dependency{
			Name:   dep.Name,
			Source: dep.Source,
			Ref:    dep.Ref,
			Digest: digest,
		}
		if prev != nil {
			if old := prev.Dependency(dep.Name); old != nil && old.Source == dep.Source && old.Ref == dep.Ref && old.Digest == digest {
				entry.ManifestDigest = old.ManifestDigest
			}
		}
		lock.Dependencies = append(lock.Dependencies, entry)
	}
	return lock, nil
}

// MismatchError lists every difference found between a lock file and the
// content of a pack.
type MismatchError struct {
	Problems []string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("%s does not match the pack content: %s", FileName, strings.Join(e.Problems, "; "))
}

// Verify compares the lock file of the pack in dir with the pack's current
// content. It returns a *MismatchError if they differ.
func Verify(dir string, metadata *pack.Metadata) (*Lock, error) {
	lock, err := Read(dir)
	if err != nil {
		return nil, err
	}
	current, err := Generate(dir, metadata, lock)
	if err != nil {
		return lock, err
	}
	return lock, Compare(lock, current)
}

// Compare returns a *MismatchError describing the differences between the
// expected (recorded) and the actual lock, or nil if they match.
func Compare(expected, actual *Lock) error {
	var problems []string
	if expected.Pack.Name != actual.Pack.Name {
		problems = append(problems, fmt.Sprintf("pack name is %q, locked %q", actual.Pack.Name, expected.Pack.Name))
	}
	if expected.Pack.Digest != actual.Pack.Digest {
		problems = append(problems, fmt.Sprintf("pack content digest is %s, locked %s", actual.Pack.Digest, expected.Pack.Digest))
	}
	for _, a := range actual.Dependencies {
		e := expected.Dependency(a.Name)
		switch {
		case e == nil:
			problems = append(problems, fmt.Sprintf("dependency %q is not locked", a.Name))
		case e.Source != a.Source || e.Ref != a.Ref:
			problems = append(problems, fmt.Sprintf("dependency %q source changed from %s@%s to %s@%s", a.Name, e.Source, e.Ref, a.Source, a.Ref))
		case e.Digest != a.Digest:
			problems = append(problems, fmt.Sprintf("dependency %q digest is %s, locked %s", a.Name, a.Digest, e.Digest))
		}
	}
	for _, e := range expected.Dependencies {
		if actual.Dependency(e.Name) == nil {
			problems = append(problems, fmt.Sprintf("locked dependency %q is no longer declared", e.Name))
		}
	}
	if len(problems) > 0 {
		return &MismatchError{Problems: problems}
	}
	return nil
}
