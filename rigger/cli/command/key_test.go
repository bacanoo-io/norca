// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"testing"
)

// A key pair is written whole or not at all: when one of its two files is
// there already, that file stays as it is and the other is not left behind,
// and the same name works once the file is out of the way.
func TestRunKey_GenerateOverAFile(t *testing.T) {
	const taken = "not a key\n"

	for _, ext := range []string{".key", ".pub"} {
		t.Run("the "+ext+" file is there", func(t *testing.T) {
			t.Setenv(passwordEnv, "secret")

			out := filepath.Join(t.TempDir(), "acme")
			writeFile(t, out+ext, taken)

			riggerRunErr(t, "acme"+ext, cmdKey, "generate", "-out", out)

			entries, err := os.ReadDir(filepath.Dir(out))
			if err != nil {
				t.Fatal(err)
			}

			if len(entries) != 1 || entries[0].Name() != "acme"+ext {
				t.Fatalf("the directory has %v, want acme%s alone: half a key pair was left", entries, ext)
			}

			if data, err := os.ReadFile(out + ext); err != nil || string(data) != taken {
				t.Fatalf("acme%s = %q, %v: the file that was there changed", ext, data, err)
			}

			if err := os.Remove(out + ext); err != nil {
				t.Fatal(err)
			}

			riggerRun(t, exitOK, []string{wroteLock, "key sha256:"}, cmdKey, "generate", "-out", out)
		})
	}
}
