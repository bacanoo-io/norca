// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package lang loads a module of Rigger: it reads the files of a module from an
// fs.FS, checks their names (design 4.5), parses them, and decodes what can
// be decoded before any value is known: the metadata, the variables, the
// rules, and the blocks of every file against its schema (design 4.1).
package lang

import (
	"io/fs"

	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca/rigger/module"
)

// labelName is the name of the label of a block that has one.
const labelName = "name"

// Module is a loaded module.
type Module struct {
	Metadata  module.Metadata
	Tabs      []module.Tab
	Variables []*Variable
	Rules     []*Rule
	// Locals, Checks, Outputs and Calls are the blocks of locals.hcl,
	// outputs.hcl and modules.hcl, decoded when the module is evaluated.
	Locals  []*hcl.Block
	Checks  []*hcl.Block
	Outputs []*hcl.Block
	Calls   []*hcl.Block
	// Resources are the resource files, one resource each.
	Resources []*Resource
	// Examples and Tests are the paths of the value files under examples/
	// and of the test files under tests/.
	Examples []string
	Tests    []string
	// Sources are the bytes of the parsed files by name, for diagnostics and
	// for runtime references (design 4.2). The files of a child module are
	// named from the directory of the module that was loaded first:
	// ../lib/placement/outputs.hcl.
	Sources map[string][]byte
	// Children are the child modules of modules.hcl, loaded.
	Children []*Child
	// FS holds the files of the module, for file() and templatefile();
	// Root the files it was loaded from, Dir its directory there.
	FS   fs.FS
	Root fs.FS
	// Dir is the directory of the module in the root it was loaded from.
	Dir string
	// Prefix names the files of the module in diagnostics (LoadAs): the
	// address of a fetched module, the source of a local one of a stack;
	// empty for a module loaded with LoadAt. A load again with the same
	// root, directory and prefix gives the same module (rig-integration.md
	// 4.6d: the evaluation in a child process).
	Prefix string
}

// Child is a child module: a module block of modules.hcl and the module it
// calls (design 6.5).
type Child struct {
	// Name is the label of the block: the name module.<name> reads.
	Name string
	// Source is the source as written.
	Source string
	// Args are the values the call gives the variables of the child.
	Args   hcl.Attributes
	Block  *hcl.Block
	Module *Module
}

// Resource is the one resource of a resource file.
type Resource struct {
	Type  ResourceType
	Name  string
	Block *hcl.Block
}

// Variable returns the variable named name.
func (m *Module) Variable(name string) (*Variable, bool) {
	for _, v := range m.Variables {
		if v.Name == name {
			return v, true
		}
	}

	return nil, false
}
