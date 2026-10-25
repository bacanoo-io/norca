package policy

import "testing"

// FuzzParseDocument exercises the policy parser (JSON and the in-house HCL
// tokenizer/parser, auto-detected) against arbitrary input. The contract is that
// it always either returns a policy or an error — never panics, hangs, or returns
// (nil, nil). Attacker-supplied policy documents make this a real surface.
func FuzzParseDocument(f *testing.F) {
	seeds := []string{
		`{"path":{"secret/*":{"capabilities":["read","list"]}}}`,
		`path "secret/data/*" { capabilities = ["read"] }`,
		`path "a" { capabilities = ["create","update","delete","list","read","sudo","deny"] }`,
		`{}`,
		``,
		`{`,
		`path`,
		`path "" {}`,
		`path "x" { capabilities = [ }`,
		"path \"x\" { capabilities = [\"read\"] }\n# comment\npath \"y\" { capabilities = [\"list\"] }",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data string) {
		pol, err := ParseDocument("fuzz", []byte(data))
		if err == nil && pol == nil {
			t.Fatalf("ParseDocument returned (nil, nil) for %q", data)
		}
	})
}
