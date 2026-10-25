// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package pack

import (
	"errors"
	"testing"

	"github.com/shoenig/test/must"
)

const testMetadataWithSchema = `
app {
  url = "https://example.com"
}
pack {
  name    = "web"
  version = "1.0.0"
}
variables_schema = file("schemas/../values.schema.json")
regions          = ["eu-aws", "eu-gcp"]
`

func TestDecodeMetadata_File(t *testing.T) {
	var read []string
	md, err := DecodeMetadata("metadata.hcl", []byte(testMetadataWithSchema), func(name string) ([]byte, error) {
		read = append(read, name)
		if name == "values.schema.json" {
			return []byte(`{"type":"object"}`), nil
		}
		return nil, errors.New("not found")
	})
	must.NoError(t, err)
	must.Eq(t, []string{"values.schema.json"}, read)
	must.Eq(t, `{"type":"object"}`, md.VariablesSchema)
	must.Eq(t, []string{"eu-aws", "eu-gcp"}, md.Regions)
	must.Eq(t, "web", md.Pack.Name)

	// Packs without the Norca extensions decode as before.
	md, err = DecodeMetadata("metadata.hcl", []byte(`
app {
  url = ""
}
pack {
  name    = "plain"
  version = "0.1.0"
}`), nil)
	must.NoError(t, err)
	must.Eq(t, "", md.VariablesSchema)
	must.Nil(t, md.Regions)
}

func TestDecodeMetadata_FileStaysInPack(t *testing.T) {
	for _, p := range []string{"../secret.json", "/etc/passwd", "a/../../x", `C:\x`, ".."} {
		src := "app {\n url = \"\"\n}\npack {\n name = \"web\"\n version = \"1\"\n}\nvariables_schema = file(" + quote(p) + ")\n"
		_, err := DecodeMetadata("metadata.hcl", []byte(src), func(string) ([]byte, error) {
			t.Fatalf("file %q must not be read", p)
			return nil, nil
		})
		must.ErrorContains(t, err, "is not a file of the pack", must.Sprint(p))
	}
}

func quote(s string) string {
	out := `"`
	for _, r := range s {
		if r == '\\' || r == '"' {
			out += `\`
		}
		out += string(r)
	}
	return out + `"`
}
