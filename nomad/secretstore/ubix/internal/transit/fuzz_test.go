package transit

import "testing"

// FuzzParseCiphertext feeds arbitrary strings to the transit ciphertext/MAC/
// signature parser (the "ubix:v<N>:<base64>" form). Callers submit these values,
// so malformed input must yield an error, never a panic; and a successful parse
// must report a valid (>= 1) version.
func FuzzParseCiphertext(f *testing.F) {
	seeds := []string{
		"",
		"ubix:v1:AAAA",
		"ubix:v0:AAAA",
		"ubix:v:AAAA",
		"ubix:v99999999999999999999:AAAA",
		"ubix:vX:AAAA",
		"ubix:v1:!!!!",
		"ubix:v1:",
		"nope",
		"ubix:v1",
		"ubix:",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		version, _, err := parseCiphertext(s)
		if err != nil {
			return
		}
		if version < 1 {
			t.Fatalf("parseCiphertext ok but version %d < 1 for %q", version, s)
		}
	})
}
