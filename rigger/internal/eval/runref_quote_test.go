// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/runref"
)

// The encoders leave the quotes of a runtime value that is a whole string
// to runref (encode.go, format.quote), and write their own around a string a
// runtime value is a part of. These tests go through both ends of that
// contract: the output of an encoder, resolved as the host resolves it.

// The runtime values of the tests, as a task template and as the client
// write them.
const (
	quoteAllocDir   = "NORCA_ALLOC_DIR"
	quoteAddress    = `{{ with nomadService "db" }}{{ with index . 0 }}{{ .Address }}:{{ .Port }}{{ end }}{{ end }}`
	quoteEnvEscaped = `{{ env "NORCA_ALLOC_DIR" | replaceAll "\\" "\\\\" | replaceAll "\"" "\\\"" }}`
	quoteJobEnv     = "${NORCA_ALLOC_DIR}"
)

// resolveAt resolves the runtime references of s for a place.
func resolveAt(t *testing.T, s string, place runref.Place) string {
	t.Helper()

	out, err := runref.Replace(s, func(p runref.Part) (string, error) { return p.Resolve(place) })
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// call calls a function of the module language.
func call(t *testing.T, fn function.Function, args ...cty.Value) cty.Value {
	t.Helper()

	v, err := fn.Call(args)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

// quoteEncoders are the encoders, all of which quote a string the same way.
func quoteEncoders() map[string]function.Function {
	return map[string]function.Function{
		"jsonencode": jsonEncode, "yamlencode": yamlEncode, "tomlencode": tomlEncode, "natsconf": natsconf,
	}
}

// service_address() and runtime_env() as the whole value of a string of an
// encoded configuration are quoted in the data of a task template, and as a
// part of a string they sit inside the quotes of the encoder.
func TestEncoders_QuoteRuntimeValueInTemplate(t *testing.T) {
	t.Parallel()

	address := call(t, serviceAddress, cty.StringVal("db"))
	dir := call(t, runtimeEnvFunc, cty.StringVal(quoteAllocDir))

	input := cty.ObjectVal(map[string]cty.Value{
		"addr": address,
		"dir":  dir,
		"url":  cty.StringVal("http://" + address.AsString() + "/x"),
	})

	for name, fn := range quoteEncoders() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := resolveAt(t, call(t, fn, input).AsString(), runref.PlaceTemplate)

			for _, want := range []string{`"` + quoteAddress + `"`, `"` + quoteEnvEscaped + `"`, `"http://` + quoteAddress + `/x"`} {
				if !strings.Contains(got, want) {
					t.Errorf("got\n%s\nwant it to contain\n%s", got, want)
				}
			}
		})
	}

	t.Run("the JSON as a whole", func(t *testing.T) {
		t.Parallel()

		got := resolveAt(t, call(t, jsonEncode, input).AsString(), runref.PlaceTemplate)
		expected := `{"addr":"` + quoteAddress + `","dir":"` + quoteEnvEscaped + `","url":"http://` + quoteAddress + `/x"}`

		if got != expected {
			t.Errorf("got\n%s\nwant\n%s", got, expected)
		}
	})
}

// Outside the data of a task template (an env of a task, an output) the
// client writes the value, and the encoded configuration still has it in
// quotes.
func TestEncoders_QuoteRuntimeValueInJob(t *testing.T) {
	t.Parallel()

	dir := call(t, runtimeEnvFunc, cty.StringVal(quoteAllocDir))

	input := cty.ObjectVal(map[string]cty.Value{
		"dir":  dir,
		"data": cty.StringVal(dir.AsString() + "/data"),
	})

	for name, fn := range quoteEncoders() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := resolveAt(t, call(t, fn, input).AsString(), runref.PlaceJob)

			for _, want := range []string{`"` + quoteJobEnv + `"`, `"` + quoteJobEnv + `/data"`} {
				if !strings.Contains(got, want) {
					t.Errorf("got\n%s\nwant it to contain\n%s", got, want)
				}
			}
		})
	}

	t.Run("the JSON is read as JSON", func(t *testing.T) {
		t.Parallel()

		got := resolveAt(t, call(t, jsonEncode, input).AsString(), runref.PlaceJob)

		var decoded map[string]string
		if err := json.Unmarshal([]byte(got), &decoded); err != nil {
			t.Fatalf("%s is not JSON: %v", got, err)
		}

		if decoded["dir"] != quoteJobEnv || decoded["data"] != quoteJobEnv+"/data" {
			t.Errorf("decoded = %v, want dir %s and data %s/data", decoded, quoteJobEnv, quoteJobEnv)
		}
	})
}

// The module of the bug report, through the evaluation: an encoded
// configuration in the env of a task and in an output.
func TestEvaluate_QuoteRuntimeValueOutsideTemplate(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"metadata.hcl": {Data: []byte("module {\n  name    = \"app\"\n  version = \"1.0.0\"\n}\n")},
		"app.norca.hcl": {Data: []byte(`job "app" {
  group "app" {
    task "app" {
      env {
        CFG = jsonencode({ dir = runtime_env("NORCA_ALLOC_DIR") })
      }
    }
  }
}
`)},
		"outputs.hcl": {Data: []byte(`output "cfg" {
  value = jsonencode({ dir = runtime_env("NORCA_ALLOC_DIR") })
}
`)},
	}

	m, diags := lang.Load(t.Context(), fsys, lang.Limits{ModuleBytes: 1 << 20, FileBytes: 1 << 20})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	vars, diags := Variables(m, nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	res, diags := Evaluate(t.Context(), m, vars, Facts{Release: "app", Namespace: "apps", Region: "eu", ModuleName: "app", ModuleVersion: "1.0.0"},
		Config{FileBytes: 1 << 20, Resources: 10, Jobs: GenericJobs})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	const expected = `{"dir":"${NORCA_ALLOC_DIR}"}`

	quoted, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}

	if spec := string(res.Resources[0].Spec); !strings.Contains(spec, `"CFG":`+string(quoted)) {
		t.Errorf("spec = %s, want CFG to be %s", spec, expected)
	}

	if len(res.Outputs) != 1 || string(res.Outputs[0].Value) != string(quoted) {
		t.Errorf("outputs = %+v, want cfg to be %s", res.Outputs, expected)
	}
}
