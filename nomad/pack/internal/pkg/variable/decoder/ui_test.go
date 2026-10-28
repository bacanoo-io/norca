// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package decoder

import (
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

func TestDecoder_FormBlocks(t *testing.T) {
	ci.Parallel(t)

	v, diags := DecodeVariableBlock(testGetHCLBlock(t, testLoadPackFile(t, []byte(`variable "redis_password" {
	type = string
	ui {
		tab     = "Security"
		group   = "Authentication"
		order   = 2
		label   = "Redis Password"
		show_if = { field = "auth_enabled", equals = true }
	}
	secret {
		path = "nomad/jobs/redis"
	}
}`))))
	must.False(t, diags.HasErrors(), must.Sprint(diags))
	must.Eq(t, map[string]any{
		"x-ui-tab":         "security",
		"x-ui-group":       "Authentication",
		"x-ui-order":       2,
		"title":            "Redis Password",
		"x-ui-show-if":     map[string]any{"field": "auth_enabled", "equals": true},
		"x-ui-secret-path": "nomad/jobs/redis",
		"x-ui-secret-item": "redis_password",
	}, v.UI)

	v, diags = DecodeVariableBlock(testGetHCLBlock(t, testLoadPackFile(t, []byte(`variable "mode" {
	type    = string
	default = "standalone"
	ui {
		options       = ["standalone", "replica"]
		option_labels = { replica = "Primary + Replicas" }
		widget        = "select"
		presets       = [1, 3]
		unit          = "nodes"
		show_if       = "enabled"
	}
	validation {
		min        = 1
		max        = 9.5
		pattern    = "^[a-z]+$"
		min_length = 2
		max_items  = 4
		integer    = true
	}
}`))))
	must.False(t, diags.HasErrors(), must.Sprint(diags))
	must.Eq(t, map[string]any{
		"enum":             []any{"standalone", "replica"},
		"x-ui-enum-labels": map[string]any{"replica": "Primary + Replicas"},
		"x-ui-widget":      "select",
		"minimum":          1,
		"maximum":          9.5,
		"pattern":          "^[a-z]+$",
		"minLength":        2,
		"maxItems":         4,
		"type":             "integer",
		"x-ui-presets":     []any{1, 3},
		"x-ui-unit":        "nodes",
		"x-ui-show-if":     map[string]any{"field": "enabled"},
	}, v.UI)

	v, diags = DecodeVariableBlock(testGetHCLBlock(t, testLoadPackFile(t, []byte(`variable "plain" {}`))))
	must.False(t, diags.HasErrors())
	must.Nil(t, v.UI)
}

func TestDecoder_FormBlocksInvalid(t *testing.T) {
	ci.Parallel(t)

	cases := map[string]string{
		"unknown widget": `ui { widget = "dial" }`,
		"tab type":       `ui { tab = 3 }`,
		"order type":     `ui { order = "first" }`,
		"empty options":  `ui { options = [] }`,
		"show_if tests":  `ui { show_if = { field = "a", equals = 1, in = [1] } }`,
		"show_if field":  `ui { show_if = { equals = 1 } }`,
		"unknown attr":   `ui { colour = "red" }`,
		"two ui blocks":  "ui {}\nui {}",
		"secret path":    `secret { item = "password" }`,
		"secret empty":   `secret { path = "" }`,
		"min in ui":      `ui { min = 1 }`,
		"bad pattern":    `validation { pattern = "(" }`,
		"length type":    `validation { min_length = 1.5 }`,
		"max type":       `validation { max = "9" }`,
		"integer type":   `validation { integer = 1 }`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			ci.Parallel(t)
			_, diags := DecodeVariableBlock(testGetHCLBlock(t, testLoadPackFile(t, []byte("variable \"v\" {\n"+body+"\n}"))))
			must.True(t, diags.HasErrors())
		})
	}
}
