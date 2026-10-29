// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package schema

import "github.com/hashicorp/hcl/v2"

const (
	VariableAttributeType        = "type"
	VariableAttributeDefault     = "default"
	VariableAttributeDescription = "description"

	// The install form blocks of a variable (Norca extension).
	VariableBlockUI         = "ui"
	VariableBlockSecret     = "secret"
	VariableBlockValidation = "validation"
)

// VariableFileSchema defines the hcl.BlockHeaderSchema for each root variable
// block. It allows us to capture the label for use as the variable name.
var VariableFileSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       "variable",
			LabelNames: []string{"name"},
		},
		// Rules on the variables of the pack (Norca extension), read by the
		// rules package.
		{
			Type:       "rule",
			LabelNames: []string{"name"},
		},
	},
}

// VariableBlockSchema defines the hcl.BodySchema for a root variable block. It
// allows us to decode blocks effectively.
var VariableBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: VariableAttributeDescription},
		{Name: VariableAttributeDefault},
		{Name: VariableAttributeType},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: VariableBlockUI},
		{Type: VariableBlockSecret},
		{Type: VariableBlockValidation},
	},
}

// VariableUIBlockSchema is the schema of the ui block of a variable: how
// the install form shows it.
var VariableUIBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "tab"},
		{Name: "group"},
		{Name: "order"},
		{Name: "label"},
		{Name: "widget"},
		{Name: "unit"},
		{Name: "step"},
		{Name: "presets"},
		{Name: "options"},
		{Name: "option_labels"},
		{Name: "show_if"},
	},
}

// VariableValidationBlockSchema is the schema of the validation block of a
// variable: constraints on its value, checked by the CLI and the servers.
var VariableValidationBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "min"},
		{Name: "max"},
		{Name: "pattern"},
		{Name: "min_length"},
		{Name: "max_length"},
		{Name: "min_items"},
		{Name: "max_items"},
		{Name: "integer"},
		{Name: "unique"},
		{Name: "item_pattern"},
		{Name: "item_min_length"},
	},
}

// VariableSecretBlockSchema is the schema of the secret block of a
// variable: the item of a Variable of the pack that receives the value.
var VariableSecretBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "path", Required: true},
		{Name: "item"},
	},
}
