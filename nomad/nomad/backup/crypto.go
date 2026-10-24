// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"filippo.io/age"
)

// Encryptor encrypts the objects of a backup for a destination. Every object
// is encrypted with authenticated encryption, so a modified object fails to
// decrypt.
type Encryptor interface {
	// Mode is the encryption mode recorded in the manifest.
	Mode() string

	// Fingerprint identifies the keys the objects are encrypted for, without
	// revealing them.
	Fingerprint() string

	// Encrypt returns a writer that encrypts into dst. Closing it flushes
	// the last chunk; it does not close dst.
	Encrypt(dst io.Writer) (io.WriteCloser, error)
}

// KeyWrapper is the seam of the reserved "kms" mode: envelope encryption
// with a per-backup data key that a cloud KMS wraps and unwraps. A KMS
// implementation would encrypt objects with the data key (for example with
// age's file key mechanism) and store the wrapped key in the manifest, so
// the server can decrypt backups for browsing without an administrator.
type KeyWrapper interface {
	// GenerateDataKey returns a new data key and its wrapped form.
	GenerateDataKey() (plaintext, wrapped []byte, err error)

	// Decrypt unwraps a wrapped data key.
	Decrypt(wrapped []byte) ([]byte, error)
}

// NewEncryptor returns the encryptor of an encryption configuration.
func NewEncryptor(cfg *EncryptionConfig) (Encryptor, error) {
	if cfg == nil {
		return nil, errors.New("missing encryption configuration")
	}
	switch cfg.Mode {
	case "", EncryptionModeAge:
		recipients, err := parseRecipients(cfg.Recipients)
		if err != nil {
			return nil, err
		}
		return &ageEncryptor{
			recipients:  recipients,
			fingerprint: RecipientsFingerprint(cfg.Recipients),
		}, nil
	case EncryptionModeKMS:
		return nil, errKMSNotImplemented
	default:
		return nil, fmt.Errorf("invalid encryption mode %q", cfg.Mode)
	}
}

type ageEncryptor struct {
	recipients  []age.Recipient
	fingerprint string
}

func (a *ageEncryptor) Mode() string        { return EncryptionModeAge }
func (a *ageEncryptor) Fingerprint() string { return a.fingerprint }

func (a *ageEncryptor) Encrypt(dst io.Writer) (io.WriteCloser, error) {
	return age.Encrypt(dst, a.recipients...)
}

// parseRecipients parses age public keys.
func parseRecipients(keys []string) ([]age.Recipient, error) {
	if len(keys) == 0 {
		return nil, errors.New("no age recipients")
	}
	recipients, err := age.ParseRecipients(strings.NewReader(strings.Join(keys, "\n")))
	if err != nil {
		return nil, fmt.Errorf("invalid age recipient: %w", err)
	}
	return recipients, nil
}

// RecipientsFingerprint returns a short, order independent fingerprint of a
// set of age recipients.
func RecipientsFingerprint(keys []string) string {
	sorted := slices.Clone(keys)
	for i := range sorted {
		sorted[i] = strings.TrimSpace(sorted[i])
	}
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:8])
}

// Identities are the age private keys supplied by an administrator to read
// a backup. They are only held for the duration of a request.
type Identities []age.Identity

// ParseIdentities parses the content of an age identity file. The error
// never contains key material.
func ParseIdentities(content string) (Identities, error) {
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("an age identity is required to decrypt the backup")
	}
	ids, err := age.ParseIdentities(strings.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("invalid age identity: %w", err)
	}
	return ids, nil
}

// decrypt returns a reader of the plaintext of an encrypted object.
func (ids Identities) decrypt(src io.Reader) (io.Reader, error) {
	r, err := age.Decrypt(src, ids...)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, errors.New("the age identity does not match the recipients of the backup")
		}
		return nil, fmt.Errorf("failed to decrypt backup object: %w", err)
	}
	return r, nil
}
