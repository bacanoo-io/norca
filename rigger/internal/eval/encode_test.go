// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package eval

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/bacanoo-io/norca/rigger/runref"
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

// special are characters that a quoted string of a format does not take as
// they are: the bell, DEL, NEL and another control character past the first
// 128, the line and the paragraph separator, the byte order mark, and a
// code point that is no character.
var special = string([]rune{0x07, 0x7f, 0x85, 0x90, 0x2028, 0x2029, 0xfeff, 0xfffe})

// Each format has its escapes for the characters it does not read as they
// are. JSON, YAML and TOML read \uXXXX: a reader of YAML refuses a control
// character, and reads NEL and the separators as a line break. NATS reads
// \xHH, one byte, and no escape of a code point: what is longer than a byte
// stays as it is, which NATS reads in a string. A key of NATS has no
// escapes at all.
func TestEncoders_Characters(t *testing.T) {
	t.Parallel()

	value := cty.ObjectVal(map[string]cty.Value{"s": cty.StringVal("a" + special + "b"), "k\a": cty.True})

	const byCode = `"a\u0007\u007f\u0085\u0090\u2028\u2029\ufeff\ufffeb"`

	tests := []struct {
		name     string
		fn       function.Function
		expected string
	}{
		{name: "natsconf", fn: natsconf, expected: "\"k\a\": true\ns: " + `"a\x07\x7f` + special[2:] + "b\"\n"},
		{name: "jsonencode", fn: jsonEncode, expected: `{"k\u0007":true,"s":` + byCode + `}`},
		{name: "yamlencode", fn: yamlEncode, expected: `"k\u0007": true` + "\ns: " + byCode + "\n"},
		{name: "tomlencode", fn: tomlEncode, expected: `"k\u0007" = true` + "\ns = " + byCode + "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := tt.fn.Call([]cty.Value{value})
			if err != nil {
				t.Fatal(err)
			}

			if got := out.AsString(); got != tt.expected {
				t.Errorf("got\n%q\nwant\n%q", got, tt.expected)
			}
		})
	}
}

// A number is written without an exponent: NATS reads 1e-07 as a string and
// refuses 1.5e-05, and a reader of YAML 1.1 takes 1e-05 for a string. NATS
// and TOML read a whole number into 64 bits and refuse a larger one, which
// is written as a float for them.
func TestEncoders_Numbers(t *testing.T) {
	t.Parallel()

	value := cty.ObjectVal(map[string]cty.Value{
		"big":   cty.MustParseNumberVal("2500000.5"),
		"huge":  cty.MustParseNumberVal("1.5e30"),
		"max":   cty.MustParseNumberVal("9223372036854775807"),
		"min":   cty.MustParseNumberVal("-9223372036854775808"),
		"neg":   cty.MustParseNumberVal("-0.5"),
		"small": cty.MustParseNumberVal("0.00001"),
		"tiny":  cty.MustParseNumberVal("1e-7"),
		"whole": cty.NumberIntVal(12),
		"wide":  cty.MustParseNumberVal("9223372036854775808"),
		"zero":  cty.MustParseNumberVal("-0.0"),
	})

	// The lines of the formats that have one number on a line, and the
	// two numbers that NATS and TOML read as floats.
	lines := func(sep, huge, wide string) string {
		return "big" + sep + "2500000.5\nhuge" + sep + huge + "\nmax" + sep + "9223372036854775807\nmin" + sep + "-9223372036854775808\n" +
			"neg" + sep + "-0.5\nsmall" + sep + "0.00001\ntiny" + sep + "0.0000001\nwhole" + sep + "12\nwide" + sep + wide + "\nzero" + sep + "0\n"
	}

	const (
		huge = "1500000000000000000000000000000"
		wide = "9223372036854775808"
	)

	tests := []struct {
		name     string
		fn       function.Function
		expected string
	}{
		{name: "natsconf", fn: natsconf, expected: lines(": ", huge+".0", wide+".0")},
		{
			name: "jsonencode", fn: jsonEncode,
			expected: `{"big":2500000.5,"huge":` + huge + `,"max":9223372036854775807,"min":-9223372036854775808,` +
				`"neg":-0.5,"small":0.00001,"tiny":0.0000001,"whole":12,"wide":` + wide + `,"zero":0}`,
		},
		{name: "yamlencode", fn: yamlEncode, expected: lines(": ", huge, wide)},
		{name: "tomlencode", fn: tomlEncode, expected: lines(" = ", huge+".0", wide+".0")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := tt.fn.Call([]cty.Value{value})
			if err != nil {
				t.Fatal(err)
			}

			if got := out.AsString(); got != tt.expected {
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

// No format has a way to write an infinite number, what 1/0 and log(0, 10)
// give: it is an error, with the place of the number.
func TestEncoders_InfiniteNumber(t *testing.T) {
	t.Parallel()

	value := cty.ObjectVal(map[string]cty.Value{"a": cty.TupleVal([]cty.Value{cty.NumberIntVal(1), cty.NegativeInfinity})})

	const want = "a: [1]: the number is infinite and can't be written"

	for _, fn := range []function.Function{natsconf, jsonEncode, yamlEncode, tomlEncode} {
		if out, err := fn.Call([]cty.Value{value}); err == nil || err.Error() != want {
			t.Errorf("got %#v, %v, want the error %q", out, err, want)
		}
	}
}
