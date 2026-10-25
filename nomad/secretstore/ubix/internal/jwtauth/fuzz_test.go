package jwtauth

import "testing"

// FuzzSplitJWT feeds arbitrary strings to the compact-JWS splitter. A malformed
// token must produce an error, never a panic — this runs before any signature
// check, on fully untrusted input.
func FuzzSplitJWT(f *testing.F) {
	seeds := []string{
		"",
		"a.b.c",
		"a.b",
		"a.b.c.d",
		"...",
		"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhIn0.c2ln",
		"@.@.@",
		"eyJ9.eyJ9.",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		header, claims, signingInput, _, err := splitJWT(raw)
		if err != nil {
			return
		}
		// On success the pieces must be usable.
		if header == nil || claims == nil {
			t.Fatalf("splitJWT ok but nil header/claims for %q", raw)
		}
		if len(signingInput) == 0 {
			t.Fatalf("splitJWT ok but empty signing input for %q", raw)
		}
	})
}

// FuzzParseJWKS feeds arbitrary bytes to the JWKS parser. It must return an error
// or a (possibly empty) key slice, never panic — JWKS documents are fetched from
// a configured but externally-controlled endpoint.
func FuzzParseJWKS(f *testing.F) {
	seeds := []string{
		`{"keys":[]}`,
		`{"keys":[{"kty":"RSA","n":"AQAB","e":"AQAB"}]}`,
		`{"keys":[{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}]}`,
		`{"keys":[{"kty":"oct"}]}`,
		`{}`,
		``,
		`{"keys":`,
		`null`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseJWKS(data) // must not panic
	})
}
