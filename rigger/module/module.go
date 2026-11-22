// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package module describes a module of Rigger as plain data: its metadata, the
// tabs of its install form and its variables (design 4.4, 4.5). The engine
// fills these from metadata.hcl and variables.hcl; a host shows them, for
// example in rigger module show or in the install form of the UI.
package module

// Metadata is the module block of metadata.hcl.
type Metadata struct {
	// Name is the name of the module, unique in its registry.
	Name string `json:"name"`
	// Version is the semver version of the module; empty for a module of a
	// stack's own (design 6.4).
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	URL         string `json:"url,omitempty"`
	// License is the license of what the module installs.
	License  string   `json:"license,omitempty"`
	Category string   `json:"category,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
	// Icon is the path of the icon in the module (design 7.4).
	Icon string `json:"icon,omitempty"`
	// Library reports a module without resources, which only other modules
	// call (design 6.5).
	Library  bool     `json:"library,omitempty"`
	Requires Requires `json:"requires"`
}

// Requires are the requirements of a module on the cluster and on other
// modules (design 6.5).
type Requires struct {
	// Norca is a version constraint on the cluster, such as ">= 0.3.0".
	Norca string `json:"norca,omitempty"`
	// Features are features the cluster must announce.
	Features []string `json:"features,omitempty"`
	// Modules are modules a stack must install next to this one.
	Modules []RequiredModule `json:"modules,omitempty"`
}

// RequiredModule is a module that must be installed next to another.
type RequiredModule struct {
	// Name is the label of the requirement: the name of the module, or a
	// name of the module's own for it when Source is set.
	Name string `json:"name"`
	// Source is the address of the required module, such as
	// "example/rustfs"; empty means the module Name of the same registry.
	Source string `json:"source,omitempty"`
	// Version is a version constraint.
	Version string `json:"version,omitempty"`
	// When is the variable that makes the requirement apply; empty for
	// always.
	When string `json:"when,omitempty"`
}

// Tab is a tab of the installation form.
type Tab struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// Variable describes an input of a module.
type Variable struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Type is the type constraint as written, such as "list(string)".
	Type string `json:"type"`
	// Required reports a variable without a default: a value must be given.
	Required bool `json:"required,omitempty"`
	// Sensitive reports a value that never appears in a plan, a log or a
	// revision (design 6.3).
	Sensitive bool `json:"sensitive,omitempty"`
}
