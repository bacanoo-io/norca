// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// The attributes of a module block that are not values for the child.
const (
	attrSource  = "source"
	attrVersion = "version"
)

// loadChildren loads the child modules of m, called from the module blocks
// of its modules.hcl. Only local sources are known yet: ./x and ../x,
// relative to m, inside root (design 7.1; the registries come in phase 3).
func loadChildren(ctx context.Context, root fs.FS, m *Module, prefix string, limits Limits, stack []string) hcl.Diagnostics {
	var diags hcl.Diagnostics

	for _, b := range m.Calls {
		if i := slices.IndexFunc(m.Children, func(c *Child) bool { return c.Name == b.Labels[0] }); i >= 0 {
			diags = append(diags, errorAt(b.LabelRanges[0], "Duplicate module",
				fmt.Sprintf("module.%s is called already at %s.", b.Labels[0], m.Children[i].Block.DefRange)))

			continue
		}

		child, moreDiags := loadChild(ctx, root, m, b, prefix, limits, stack)
		diags = append(diags, moreDiags...)

		if child != nil {
			m.Children = append(m.Children, child)
		}

		if limits.loaded.over || ctx.Err() != nil {
			break
		}
	}

	return diags
}

func loadChild(ctx context.Context, root fs.FS, m *Module, b *hcl.Block, prefix string, limits Limits, stack []string) (*Child, hcl.Diagnostics) {
	attrs, diags := b.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}

	src, moreDiags := callSource(b, attrs)
	diags = append(diags, moreDiags...)

	if moreDiags.HasErrors() {
		return nil, diags
	}

	dir := path.Join(m.Dir, src)
	rng := attrs[attrSource].Expr.Range()

	switch {
	case dir == ".." || strings.HasPrefix(dir, "../"):
		return nil, append(diags, errorAt(rng, "Source outside the root",
			src+" is outside the files the module was loaded from; a child module is read from the same source."))
	case slices.Contains(stack, dir):
		return nil, append(diags, errorAt(rng, "Modules call each other",
			src+" is a module that calls this one already."))
	case limits.Depth > 0 && len(stack) >= limits.Depth:
		return nil, append(diags, errorAt(rng, "Child modules too deep",
			fmt.Sprintf("The child modules are nested more than %d deep, the limit of the engine.", limits.Depth)))
	}

	if err := CheckPath(root, m.Dir, src); err != nil {
		return nil, append(diags, errorAt(rng, "Invalid source", err.Error()+"."))
	}

	mod, moreDiags := loadTree(ctx, root, dir, path.Join(prefix, src), limits, stack)
	diags = append(diags, moreDiags...)

	delete(attrs, attrSource)
	delete(attrs, attrVersion)

	return &Child{Name: b.Labels[0], Source: src, Args: attrs, Block: b, Module: mod}, diags
}

// callSource returns the source of a module block: a constant string, local.
func callSource(b *hcl.Block, attrs hcl.Attributes) (string, hcl.Diagnostics) {
	attr, ok := attrs[attrSource]
	if !ok {
		return "", hcl.Diagnostics{errorAt(b.DefRange, "Missing source", "A module block names its source.")}
	}

	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		return "", diags
	}

	src, ok := stringOf(v)
	if !ok {
		return "", hcl.Diagnostics{errorAt(attr.Expr.Range(), "Invalid source", "The source of a module is a string.")}
	}

	if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
		return "", hcl.Diagnostics{errorAt(attr.Expr.Range(), "Source not supported yet",
			src+" is not a local source (./ or ../). Modules from registries come with phase 3.")}
	}

	if v, ok := attrs[attrVersion]; ok {
		return "", hcl.Diagnostics{errorAt(v.NameRange, "Version of a local source",
			"A local source is used as it is; it has no version.")}
	}

	return src, nil
}
