package policy

import (
	"errors"
	"testing"
)

func TestParseHCL(t *testing.T) {
	doc := `
# a comment
path "secret/data/app/*" {
  capabilities = ["read", "list"]
}

// another comment
path "secret/data/admin/*" {
  capabilities = ["deny"]
}

/* block
   comment */
path "sys/health" {
  capabilities = ["read"]
}
`
	p, err := ParseHCL("mixed", []byte(doc))
	if err != nil {
		t.Fatalf("ParseHCL: %v", err)
	}
	acl := NewACL(p)

	if !acl.Allows("secret/data/app/db", Read) || !acl.Allows("secret/data/app/db", List) {
		t.Fatal("app read/list not granted")
	}
	if acl.Allows("secret/data/app/db", Create) {
		t.Fatal("create wrongly granted")
	}
	if acl.Allows("secret/data/admin/x", Read) {
		t.Fatal("deny not applied to admin path")
	}
	if !acl.Allows("sys/health", Read) {
		t.Fatal("sys/health read not granted")
	}
}

func TestParseHCLTrailingCommaAndSpacing(t *testing.T) {
	// Loose spacing and a trailing comma should parse.
	p, err := ParseHCL("x", []byte(`path "a/*"{capabilities=["read",]}`))
	if err != nil {
		t.Fatalf("ParseHCL: %v", err)
	}
	if len(p.Rules) != 1 || p.Rules[0].Path != "a/*" {
		t.Fatalf("rules = %+v", p.Rules)
	}
}

func TestParseHCLUnknownCapability(t *testing.T) {
	_, err := ParseHCL("x", []byte(`path "a" { capabilities = ["frobnicate"] }`))
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("want ErrUnknownCapability, got %v", err)
	}
}

func TestParseHCLMalformed(t *testing.T) {
	cases := []string{
		`path "a" { capabilities = ["read"`,    // missing ] and }
		`path "a" `,                            // missing block
		`role "a" { capabilities = ["read"] }`, // wrong keyword
		`path a { capabilities = ["read"] }`,   // unquoted path
		`path "a" { caps = ["read"] }`,         // wrong inner keyword
		`path "unterminated`,                   // unterminated string
	}
	for _, c := range cases {
		if _, err := ParseHCL("x", []byte(c)); !errors.Is(err, ErrMalformedPolicy) {
			// Unknown-capability is a different sentinel; everything here is malformed.
			t.Errorf("ParseHCL(%q): want ErrMalformedPolicy, got %v", c, err)
		}
	}
}

func TestParseHCLEmpty(t *testing.T) {
	p, err := ParseHCL("empty", []byte("  \n # just a comment\n"))
	if err != nil {
		t.Fatalf("ParseHCL empty: %v", err)
	}
	if len(p.Rules) != 0 {
		t.Fatalf("empty policy has rules: %+v", p.Rules)
	}
}

func TestParseDocumentAutoDetects(t *testing.T) {
	json := `{"path":{"secret/data/a/*":{"capabilities":["read"]}}}`
	hcl := `path "secret/data/a/*" { capabilities = ["read"] }`

	for name, doc := range map[string]string{"json": json, "hcl": hcl} {
		p, err := ParseDocument(name, []byte(doc))
		if err != nil {
			t.Fatalf("ParseDocument(%s): %v", name, err)
		}
		if !NewACL(p).Allows("secret/data/a/x", Read) {
			t.Fatalf("%s policy did not grant read", name)
		}
	}

	// Leading whitespace before JSON is still detected as JSON.
	if _, err := ParseDocument("x", []byte("\n  "+json)); err != nil {
		t.Fatalf("ParseDocument with leading space: %v", err)
	}
}
