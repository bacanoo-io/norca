// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"

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

func TestNatsconf(t *testing.T) {
	t.Parallel()

	env := cty.StringVal(runref.Part{Kind: runref.KindEnv, Name: "NORCA_ALLOC_DIR"}.String() + "/data")
	secret := cty.StringVal(runref.Part{Kind: runref.KindSecret, Name: "nats/users/app", Item: "password"}.String())
	routes := cty.StringVal(runref.Part{Kind: runref.KindServices, Name: "nats-cluster", Scheme: "nats-route", ExcludeSelf: true}.String())

	config := cty.ObjectVal(map[string]cty.Value{
		"server_name": cty.StringVal(`a "quoted" name`),
		"port":        cty.NumberIntVal(4222),
		"debug":       cty.False,
		"tls":         cty.NullVal(cty.DynamicPseudoType),
		"jetstream": cty.ObjectVal(map[string]cty.Value{
			"store_dir":        env,
			"max_memory_store": cty.StringVal("1G"),
		}),
		"accounts": cty.ObjectVal(map[string]cty.Value{
			"my app": cty.ObjectVal(map[string]cty.Value{
				"users": cty.TupleVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
					"user": cty.StringVal("app"), "password": secret,
				})}),
			}),
		}),
		"cluster": cty.ObjectVal(map[string]cty.Value{
			"routes": cty.TupleVal([]cty.Value{cty.StringVal("nats-route://seed:6222"), routes}),
		}),
	})

	out, err := natsconf.Call([]cty.Value{config})
	if err != nil {
		t.Fatal(err)
	}

	expected := `accounts {
  "my app" {
    users: [
      {
        password: "{{ with index (norcaSecret "nats/users/app") "password" }}{{ . | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}{{ else }}{{ sprig_fail "the secret nats/users/app has no item password" }}{{ end }}"
        user: "app"
      }
    ]
  }
}
cluster {
  routes: [
    "nats-route://seed:6222"
{{ $first := true }}{{ range nomadService "nats-cluster" }}{{ if ne .AllocID (env "NORCA_ALLOC_ID") }}{{ if not $first }}
{{ end }}{{ $first = false }}"nats-route://{{ .Address }}:{{ .Port }}"{{ end }}{{ end }}
  ]
}
debug: false
jetstream {
  max_memory_store: "1G"
  store_dir: "{{ env "NORCA_ALLOC_DIR" | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}/data"
}
port: 4222
server_name: "a \"quoted\" name"
`

	if got := template(t, out.AsString()); got != expected {
		t.Errorf("natsconf() =\n%s\nwant\n%s", got, expected)
	}
}

func TestNatsconf_Errors(t *testing.T) {
	t.Parallel()

	routes := cty.StringVal(runref.Part{Kind: runref.KindServices, Name: "x"}.String())

	tests := []struct {
		name     string
		config   cty.Value
		expected string
	}{
		{name: "not an object", config: cty.StringVal("x"), expected: "the configuration is an object"},
		{name: "addresses as a string", config: cty.ObjectVal(map[string]cty.Value{"a": routes}), expected: "a: service_addresses() gives a list"},
		{
			name:     "an unknown value",
			config:   cty.ObjectVal(map[string]cty.Value{"a": cty.UnknownVal(cty.String)}),
			expected: "not known at install time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := natsconf.Call([]cty.Value{tt.config})
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Errorf("natsconf() error = %v, want it to contain %q", err, tt.expected)
			}
		})
	}
}
