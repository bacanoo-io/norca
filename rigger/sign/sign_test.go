// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sign_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/internal/readlimit"
	"github.com/bacanoo-io/norca/rigger/sign"
)

const digest = "sha256:5e7a9c1b3d5f7e9a1c3b5d7f9e1a3c5b7d9f1e3a1c3e5a7b9d1f3e5c7a9b1d3f"

func TestSignVerify(t *testing.T) {
	t.Parallel()

	key, err := sign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	other, _ := sign.GenerateKey()

	pubText, _ := sign.MarshalPublicKey(&key.PublicKey)
	otherText, _ := sign.MarshalPublicKey(&other.PublicKey)

	pub, err := sign.ParsePublicKey(pubText)
	if err != nil {
		t.Fatal(err)
	}

	otherPub, _ := sign.ParsePublicKey(otherText)

	s, err := sign.Sign(key, digest, "nats", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}

	if got := sign.Verify(digest, []sign.Signature{s}, []sign.PublicKey{otherPub, pub}); len(got) != 1 || got[0] != pub.ID {
		t.Errorf("Verify = %v, want %s", got, pub.ID)
	}

	if got := sign.Verify("sha256:00", []sign.Signature{s}, []sign.PublicKey{pub}); len(got) != 0 {
		t.Errorf("a signature of another digest verified: %v", got)
	}

	if got := sign.Verify(digest, []sign.Signature{s}, []sign.PublicKey{otherPub}); len(got) != 0 {
		t.Errorf("another key verified: %v", got)
	}

	data, _ := json.Marshal(s)
	fsys := fstest.MapFS{"m/.rigger-signatures/" + sign.FileName(pub.ID): {Data: data}}

	sigs, err := sign.ReadSignatures(fsys, "m")
	if err != nil || len(sigs) != 1 || sigs[0] != s {
		t.Errorf("ReadSignatures = %v, %v", sigs, err)
	}

	if sigs, err := sign.ReadSignatures(fsys, "none"); err != nil || len(sigs) != 0 {
		t.Errorf("ReadSignatures of a module without signatures = %v, %v", sigs, err)
	}
}

// signatureFile is a signature file of size bytes.
func signatureFile(t *testing.T, size int) *fstest.MapFile {
	t.Helper()

	empty, err := json.Marshal(sign.Signature{})
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(sign.Signature{Payload: strings.Repeat("x", size-len(empty))})
	if err != nil || len(data) != size {
		t.Fatalf("a signature of %d bytes: %d bytes, %v", size, len(data), err)
	}

	return &fstest.MapFile{Data: data}
}

// signatureDir is the directory of the signatures of the module m with n
// signatures and what else a directory may hold.
func signatureDir(t *testing.T, n int) fstest.MapFS {
	t.Helper()

	fsys := fstest.MapFS{
		"m/.rigger-signatures/README.md":      {Data: []byte("not a signature")},
		"m/.rigger-signatures/old/1.json":     {Data: []byte("not read")},
		"m/.rigger-signatures/dir.json/x.txt": {Data: []byte("a directory")},
	}

	for i := range n {
		fsys[fmt.Sprintf("m/.rigger-signatures/%04d.json", i)] = signatureFile(t, 64)
	}

	return fsys
}

// The signatures come with content fetched from others: their number and
// the size of each are bounded, and more is an error, not a part of them.
func TestReadSignatures_Bounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fsys     fstest.MapFS
		expected int
		tooLarge bool
	}{
		{name: "as many signatures as the bound", fsys: signatureDir(t, sign.MaxSignatures), expected: sign.MaxSignatures},
		{name: "one signature more", fsys: signatureDir(t, sign.MaxSignatures+1), tooLarge: true},
		{
			name:     "a signature of the size of the bound",
			fsys:     fstest.MapFS{"m/.rigger-signatures/a.json": signatureFile(t, sign.MaxSignatureBytes)},
			expected: 1,
		},
		{
			name:     "a signature one byte larger",
			fsys:     fstest.MapFS{"m/.rigger-signatures/a.json": signatureFile(t, sign.MaxSignatureBytes+1)},
			tooLarge: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sigs, err := sign.ReadSignatures(tt.fsys, "m")
			if errors.Is(err, readlimit.ErrTooLarge) != tt.tooLarge || len(sigs) != tt.expected || (err != nil) != tt.tooLarge {
				t.Errorf("ReadSignatures() = %d signatures, %v; want %d, over the bound %t", len(sigs), err, tt.expected, tt.tooLarge)
			}
		})
	}
}

func TestPrivateKey(t *testing.T) {
	t.Parallel()

	key, _ := sign.GenerateKey()

	text, err := sign.MarshalPrivateKey(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}

	back, err := sign.ParsePrivateKey(text, []byte("secret"))
	if err != nil || !back.Equal(key) {
		t.Fatalf("ParsePrivateKey = %v, %v", back, err)
	}

	if _, err := sign.ParsePrivateKey(text, []byte("wrong")); !errors.Is(err, sign.ErrPassword) {
		t.Errorf("a wrong password: %v", err)
	}
}
