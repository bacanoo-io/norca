// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package lang loads a module of Rig: it reads the files of a module from an
// fs.FS, checks their names (design 4.5), parses them, and decodes what can
// be decoded before any value is known: the metadata, the variables, the
// rules, and the blocks of every file against its schema (design 4.1).
package lang

import (
	"github.com/hashicorp/hcl/v2"

	"github.com/bacanoo-io/norca-rig/module"
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
	// for runtime references (design 4.2).
	Sources map[string][]byte
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
