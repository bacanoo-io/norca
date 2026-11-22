// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"

	"github.com/bacanoo-io/norca/rigger/module"
)

// moduleName is the name of a module or of a release: the rule of the
// deployer (design 6.4).
var moduleName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// semver is a version of a module (design 7.2).
var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// metadataFile is metadata.hcl of a module.
type metadataFile struct {
	Module *moduleBlock `hcl:"module,block"`
	UI     *uiBlock     `hcl:"ui,block"`
}

type moduleBlock struct {
	Name        string         `hcl:"name"`
	Version     string         `hcl:"version,optional"`
	Description string         `hcl:"description,optional"`
	URL         string         `hcl:"url,optional"`
	License     string         `hcl:"license,optional"`
	Category    string         `hcl:"category,optional"`
	Keywords    []string       `hcl:"keywords,optional"`
	Icon        string         `hcl:"icon,optional"`
	Library     bool           `hcl:"library,optional"`
	Requires    *requiresBlock `hcl:"requires,block"`
}

type requiresBlock struct {
	Norca    string                 `hcl:"norca,optional"`
	Features []string               `hcl:"features,optional"`
	Modules  []*requiredModuleBlock `hcl:"module,block"`
}

type requiredModuleBlock struct {
	Name    string `hcl:"name,label"`
	Source  string `hcl:"source,optional"`
	Version string `hcl:"version,optional"`
	When    string `hcl:"when,optional"`
}

type uiBlock struct {
	Tabs []*tabBlock `hcl:"tab,block"`
}

type tabBlock struct {
	Name  string `hcl:"name,label"`
	Label string `hcl:"label,optional"`
	Icon  string `hcl:"icon,optional"`
}

// decodeMetadata decodes metadata.hcl into m.
func (m *Module) decodeMetadata(file *hcl.File) hcl.Diagnostics {
	var md metadataFile

	diags := gohcl.DecodeBody(file.Body, nil, &md)
	if diags.HasErrors() {
		return diags
	}

	if md.Module == nil {
		return hcl.Diagnostics{errorAt(file.Body.MissingItemRange(), "Missing module block",
			"metadata.hcl of a module holds a module block with its name and version.")}
	}

	md2, moreDiags := md.Module.metadata(blockRange(file.Body, "module"), "module")
	m.Metadata, diags = md2, append(diags, moreDiags...)

	if md.UI != nil {
		for _, t := range md.UI.Tabs {
			m.Tabs = append(m.Tabs, module.Tab{Name: t.Name, Label: t.Label, Icon: t.Icon})
		}
	}

	return diags
}

// metadata checks the block of a module or of a stack (what) and returns
// it as metadata.
func (b *moduleBlock) metadata(rng hcl.Range, what string) (module.Metadata, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	if !moduleName.MatchString(b.Name) {
		diags = append(diags, errorAt(rng, "Invalid "+what+" name",
			fmt.Sprintf("%q is not a name: lower case letters, digits and -, at most 63.", b.Name)))
	}

	// A module of a stack's own needs no version (design 6.4); publishing
	// needs one.
	if b.Version != "" && !semver.MatchString(b.Version) {
		diags = append(diags, errorAt(rng, "Invalid version",
			fmt.Sprintf("%q is not a semver version such as 1.2.0.", b.Version)))
	}

	// A catalog filters on the category and shows an icon of each.
	if b.Category != "" && !slices.Contains(module.Categories, b.Category) {
		diags = append(diags, errorAt(rng, "Invalid category",
			fmt.Sprintf("%q is not a category: %s.", b.Category, strings.Join(module.Categories, ", "))))
	}

	md := module.Metadata{
		Name: b.Name, Version: b.Version, Description: b.Description, URL: b.URL,
		License: b.License, Category: b.Category, Keywords: b.Keywords, Icon: b.Icon, Library: b.Library,
	}

	if r := b.Requires; r != nil {
		md.Requires = module.Requires{Norca: r.Norca, Features: r.Features}
		for _, rm := range r.Modules {
			md.Requires.Modules = append(md.Requires.Modules,
				module.RequiredModule{Name: rm.Name, Source: rm.Source, Version: rm.Version, When: rm.When})
		}
	}

	return md, diags
}

// stackMetadataFile is metadata.hcl of a published stack (design 7.5).
type stackMetadataFile struct {
	Stack *moduleBlock `hcl:"stack,block"`
}

// decodeMetadata decodes metadata.hcl of a stack: its stack block, with the
// name of the rigger block and a version.
func (s *Stack) decodeMetadata(file *hcl.File) hcl.Diagnostics {
	var md stackMetadataFile

	diags := gohcl.DecodeBody(file.Body, nil, &md)
	if diags.HasErrors() {
		return diags
	}

	rng := blockRange(file.Body, "stack")

	if md.Stack == nil {
		return hcl.Diagnostics{errorAt(rng, "Missing stack block",
			"metadata.hcl of a stack holds a stack block with its name and version (design 7.5).")}
	}

	meta, moreDiags := md.Stack.metadata(rng, "stack")
	diags = append(diags, moreDiags...)

	switch {
	case meta.Version == "":
		diags = append(diags, errorAt(rng, "Missing version", "A published stack has a version (design 7.5)."))
	case meta.Library:
		diags = append(diags, errorAt(rng, "Library stack", "A stack is installed; library is an attribute of a module."))
	}

	s.Metadata = &meta

	return diags
}

// blockRange returns the range of the header of the first block of type
// typ in body, or the range where it is missing.
func blockRange(body hcl.Body, typ string) hcl.Range {
	content, _, _ := body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: typ}}})
	if len(content.Blocks) == 0 {
		return body.MissingItemRange()
	}

	return content.Blocks[0].DefRange
}
