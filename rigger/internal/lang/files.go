// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/rigger/resource"
)

// FileKind is what a file of a module holds, by its name (design 4.5).
type FileKind int

// The kinds of files of a module.
const (
	FileUnknown FileKind = iota
	FileMetadata
	FileVariables
	FileLocals
	FileModules
	FileOutputs
	FileResource
	// FileData is a file under files/, read by file() and templatefile().
	FileData
	// FileExample is a value file under examples/.
	FileExample
	// FileTest is a test file under tests/.
	FileTest
	// FileDoc is the documentation, the license and the icon.
	FileDoc
)

// ResourceType is a kind of resource as a file of a module holds it.
type ResourceType struct {
	// Kind is the kind of the resource.
	Kind resource.Kind
	// Block is the type of the one block of the file.
	Block string
}

// KindAudit is the kind of an audit declaration (design 12.8). It is not
// one of the kinds of the release API yet.
const KindAudit resource.Kind = "audit"

// resourceTypes maps the extension of a resource file to its type. The block
// of a kind is the one Norca reads for it where the design names it (a job,
// an ACL policy, a Variable); for the others it is the extension, until the
// kind is implemented (design 4.5).
var resourceTypes = map[string]ResourceType{
	"norca":       {Kind: resource.KindJob, Block: "job"},
	attrNamespace: {Kind: resource.KindNamespace, Block: attrNamespace},
	"quota":       {Kind: resource.KindQuota, Block: "quota"},
	"acl":         {Kind: resource.KindACLPolicy, Block: "policy"},
	"var":         {Kind: resource.KindVariable, Block: "variable"},
	"kind":        {Kind: resource.KindObjectKind, Block: "kind"},
	"admission":   {Kind: resource.KindAdmissionPolicy, Block: "admission"},
	"external":    {Kind: resource.KindExternalService, Block: "external"},
	"object":      {Kind: resource.KindObject, Block: "object"},
	"audit":       {Kind: KindAudit, Block: "audit"},
}

// ResourceBlocks returns the block types of the resource files, sorted.
func ResourceBlocks() []string {
	out := make([]string, 0, len(resourceTypes))
	for _, rt := range resourceTypes {
		out = append(out, rt.Block)
	}

	slices.Sort(out)

	return out
}

// resourceName is the name of a resource: the part of its file name before
// the extension, and the label of its block.
var resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// docFiles are the files of the documentation, the license and the icon.
var docFiles = map[string]bool{
	"README.md":    true,
	"CHANGELOG.md": true,
	"LICENSE":      true,
	"NOTICE":       true,
	"icon.png":     true,
}

// Classify says what the file at p (slash separated, relative to the module)
// holds. For a resource file it gives the type and the name of the
// resource. Hidden files (a name that starts with a dot) are FileDoc.
func Classify(p string) (FileKind, ResourceType, string) {
	dir, name := path.Split(p)
	if strings.HasPrefix(name, ".") {
		return FileDoc, ResourceType{}, ""
	}

	switch top, _, _ := strings.Cut(dir, "/"); top {
	case "":
		return classifyTop(name)
	case "files":
		return FileData, ResourceType{}, ""
	case "examples":
		if dir == "examples/" && strings.HasSuffix(name, ".vars.hcl") {
			return FileExample, ResourceType{}, ""
		}
	case "tests":
		if dir == "tests/" && strings.HasSuffix(name, ".riggertest.hcl") {
			return FileTest, ResourceType{}, ""
		}
	}

	return FileUnknown, ResourceType{}, ""
}

func classifyTop(name string) (FileKind, ResourceType, string) {
	switch name {
	case "metadata.hcl":
		return FileMetadata, ResourceType{}, ""
	case "variables.hcl":
		return FileVariables, ResourceType{}, ""
	case "locals.hcl":
		return FileLocals, ResourceType{}, ""
	case "modules.hcl":
		return FileModules, ResourceType{}, ""
	case "outputs.hcl":
		return FileOutputs, ResourceType{}, ""
	}

	if docFiles[name] {
		return FileDoc, ResourceType{}, ""
	}

	base, ok := strings.CutSuffix(name, ".hcl")
	if !ok {
		return FileUnknown, ResourceType{}, ""
	}

	stem, ext, found := strings.CutLast(base, ".")
	if !found {
		return FileUnknown, ResourceType{}, ""
	}

	rt, known := resourceTypes[ext]
	if !known || !resourceName.MatchString(stem) {
		return FileUnknown, ResourceType{}, ""
	}

	return FileResource, rt, stem
}
