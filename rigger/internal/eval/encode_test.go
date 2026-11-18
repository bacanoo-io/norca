// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca-rig/runref"
)

// template resolves the runtime references of s as a task template does.
func template(t *testing.T, s string) string {
	t.Helper()

	out, err := runref.Replace(s, func(p runref.Part) (string, error) { return p.Resolve(runref.PlaceTemplate) })
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// The runtime parts of the tests, and how a template writes them.
const (
	escaped = ` | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\""`
	envDir  = `{{ env "NORCA_ALLOC_DIR"` + escaped + ` }}/data`
	pw      = `"{{ with index (norcaSecret "app") "password" }}{{ .` + escaped + ` }}{{ else }}{{ sprig_fail "the secret app has no item password" }}{{ end }}"`
	routes  = `{{ $first := true }}{{ range nomadService "c" }}{{ if ne .AllocID (env "NORCA_ALLOC_ID") }}{{ if $first }}, {{ else }}, {{ end }}{{ $first = false }}"r://{{ .Address }}:{{ .Port }}"{{ end }}{{ end }}`
)

// encodeInput is the value all encoders write.
func encodeInput() cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"name":  cty.StringVal(`a "quoted"` + "\nname"),
		"port":  cty.NumberIntVal(4222),
		"ratio": cty.NumberFloatVal(0.5),
		"debug": cty.False,
		"tls":   cty.NullVal(cty.DynamicPseudoType),
		"store": cty.ObjectVal(map[string]cty.Value{
			"dir": cty.StringVal(runref.Part{Kind: runref.KindEnv, Name: "NORCA_ALLOC_DIR"}.String() + "/data"),
		}),
		"my users": cty.TupleVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
			"user": cty.StringVal("app"),
			"pw":   cty.StringVal(runref.Part{Kind: runref.KindSecret, Name: "app", Item: "password"}.String()),
		})}),
		"routes": cty.TupleVal([]cty.Value{
			cty.StringVal("r://seed:1"),
			cty.StringVal(runref.Part{Kind: runref.KindServices, Name: "c", Scheme: "r", ExcludeSelf: true}.String()),
		}),
	})
}

func TestEncoders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fn       function.Function
		expected string
	}{
		{
			name: "natsconf",
			fn:   natsconf,
			expected: `debug: false
"my users": [
  {
    pw: ` + pw + `
    user: "app"
  }
]
name: "a \"quoted\"\nname"
port: 4222
ratio: 0.5
routes: ["r://seed:1"` + routes + `]
store {
  dir: "` + envDir + `"
}
`,
		},
		{
			name: "jsonencode",
			fn:   jsonEncode,
			expected: `{"debug":false,"my users":[{"pw":` + pw + `,"user":"app"}],"name":"a \"quoted\"\nname","port":4222,"ratio":0.5,` +
				`"routes":["r://seed:1"` + strings.ReplaceAll(routes, ", ", ",") + `],"store":{"dir":"` + envDir + `"},"tls":null}`,
		},
		{
			name: "yamlencode",
			fn:   yamlEncode,
			expected: `debug: false
"my users":
  - pw: ` + pw + `
    user: "app"
name: "a \"quoted\"\nname"
port: 4222
ratio: 0.5
routes: ["r://seed:1"` + routes + `]
store:
  dir: "` + envDir + `"
tls: null
`,
		},
		{
			name: "tomlencode",
			fn:   tomlEncode,
			expected: `debug = false
name = "a \"quoted\"\nname"
port = 4222
ratio = 0.5
routes = ["r://seed:1"` + routes + `]

[store]
dir = "` + envDir + `"

[["my users"]]
pw = ` + pw + `
user = "app"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := tt.fn.Call([]cty.Value{encodeInput()})
			if err != nil {
				t.Fatal(err)
			}

			if got := template(t, out.AsString()); got != tt.expected {
				t.Errorf("got\n%s\nwant\n%s", got, tt.expected)
			}
		})
	}
}

func TestEncoders_Errors(t *testing.T) {
	t.Parallel()

	addresses := cty.StringVal(runref.Part{Kind: runref.KindServices, Name: "x"}.String())

	tests := []struct {
		name     string
		fn       function.Function
		value    cty.Value
		expected string
	}{
		{name: "natsconf of a string", fn: natsconf, value: cty.StringVal("x"), expected: "the configuration is an object"},
		{name: "tomlencode of a list", fn: tomlEncode, value: cty.EmptyTupleVal, expected: "a TOML document is an object"},
		{
			name: "addresses as a string", fn: jsonEncode,
			value: cty.ObjectVal(map[string]cty.Value{"a": addresses}), expected: "a: service_addresses() gives a list",
		},
		{
			name: "addresses before an item", fn: yamlEncode,
			value:    cty.ObjectVal(map[string]cty.Value{"a": cty.TupleVal([]cty.Value{addresses, cty.StringVal("x")})}),
			expected: "go at the end of a list",
		},
		{
			name: "null in a TOML list", fn: tomlEncode,
			value:    cty.ObjectVal(map[string]cty.Value{"a": cty.TupleVal([]cty.Value{cty.NullVal(cty.String)})}),
			expected: "TOML has no null",
		},
		{
			name: "an unknown value", fn: natsconf,
			value:    cty.ObjectVal(map[string]cty.Value{"a": cty.UnknownVal(cty.String)}),
			expected: "not known at install time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tt.fn.Call([]cty.Value{tt.value})
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Errorf("error = %v, want it to contain %q", err, tt.expected)
			}
		})
	}
}
