// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sign_test

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/rigger/sign"
)

// withScrypt returns a key file with other parameters of scrypt.
func withScrypt(t *testing.T, text []byte, n, r, p int64) []byte {
	t.Helper()

	block, _ := pem.Decode(text)
	if block == nil {
		t.Fatal("no PEM block in the key file")
	}

	var file map[string]any
	if err := json.Unmarshal(block.Bytes, &file); err != nil {
		t.Fatal(err)
	}

	kdf, _ := file["kdf"].(map[string]any)
	kdf["params"] = map[string]int64{"N": n, "r": r, "p": p}

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: data})
}

// A key file names its own parameters of scrypt, and scrypt takes 128*N*r
// bytes: one that asks for more than a key file needs is refused before it
// is tried (N of 2^30 is a terabyte, and the end of the process).
func TestParsePrivateKey_ScryptBounds(t *testing.T) {
	t.Parallel()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	text, err := sign.MarshalPrivateKey(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		n, r, p int64
	}{
		{name: "half a gigabyte", n: 1 << 19, r: 8, p: 1},
		{name: "a terabyte", n: 1 << 30, r: 8, p: 1},
		{name: "a large r", n: 32768, r: 1 << 20, p: 1},
		{name: "an r past an int32", n: 2, r: 1 << 40, p: 1},
		{name: "a large p", n: 32768, r: 8, p: 1 << 20},
		{name: "N of zero", n: 0, r: 8, p: 1},
		{name: "a negative N", n: -32768, r: 8, p: 1},
		{name: "a negative r", n: 32768, r: -8, p: 1},
		{name: "p of zero", n: 32768, r: 8, p: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			start := time.Now()

			_, err := sign.ParsePrivateKey(withScrypt(t, text, tt.n, tt.r, tt.p), []byte("secret"))
			if !errors.Is(err, sign.ErrKey) {
				t.Errorf("ParsePrivateKey() with N=%d r=%d p=%d = %v, want ErrKey", tt.n, tt.r, tt.p, err)
			}

			if took := time.Since(start); took > time.Second {
				t.Errorf("the parameters were tried for %s before they were refused", took)
			}
		})
	}

	// The parameters of the tools that write such files stay readable: of
	// this package, and N of 2^16 of cosign.
	for _, n := range []int64{32768, 65536} {
		// Another N derives another key: the password no longer opens the
		// box, which says that scrypt ran.
		if _, err := sign.ParsePrivateKey(withScrypt(t, text, n, 8, 1), []byte("secret")); n == 32768 && err != nil || n != 32768 && !errors.Is(err, sign.ErrPassword) {
			t.Errorf("ParsePrivateKey() with N=%d = %v", n, err)
		}
	}
}
