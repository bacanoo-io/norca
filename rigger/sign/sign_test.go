// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package sign_test

import (
	"encoding/json"
	"errors"
	"testing"
	"testing/fstest"

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
