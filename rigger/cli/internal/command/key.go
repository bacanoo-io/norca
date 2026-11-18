// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-rig/sign"
)

// passwordEnv holds the password of a private key.
const passwordEnv = "RIG_KEY_PASSWORD"

var (
	errPassword = errors.New(passwordEnv + " holds the password of the private key; set it (an empty value is a key without a password)")
	errKeyFlag  = errors.New("the command needs -key")
)

// runKey runs rig key generate|sign|verify (design 8.1, 10).
func runKey(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	if len(args) == 0 {
		warn(stderr, "key needs a command: generate, sign or verify")

		return exitError
	}

	cmd, args := args[0], args[1:]

	var positional []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, args = append(positional, args[0]), args[1:]
	}

	flags := flag.NewFlagSet("rig key "+cmd, flag.ContinueOnError)
	flags.SetOutput(stderr)
	key := flags.String("key", "", "sign: the private key; verify: the public key")
	out := flags.String("out", "rig", "generate: the files to write, <out>.key and <out>.pub")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	positional = append(positional, flags.Args()...)
	dir := "."

	if len(positional) > 0 {
		dir = positional[0]
	}

	return run(ctx, logger, stderr, "key."+cmd, func(ctx context.Context) error {
		switch cmd {
		case "generate":
			return generateKey(stdout, *out)
		case "sign":
			return signModule(ctx, logger, stdout, stderr, dir, *key)
		case "verify":
			return verifyModule(ctx, logger, stdout, stderr, dir, *key)
		default:
			return fmt.Errorf("unknown key command %q", cmd)
		}
	})
}

// generateKey writes a new key pair: <out>.key encrypted with the password
// of RIG_KEY_PASSWORD, and <out>.pub.
func generateKey(w io.Writer, out string) error {
	password, ok := os.LookupEnv(passwordEnv)
	if !ok {
		return errPassword
	}

	key, err := sign.GenerateKey()
	if err != nil {
		return err
	}

	private, err := sign.MarshalPrivateKey(key, []byte(password))
	if err != nil {
		return err
	}

	public, err := sign.MarshalPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}

	if err := writeNew(out+".key", private, 0o600); err != nil {
		return err
	}

	if err := writeNew(out+".pub", public, 0o644); err != nil {
		return err
	}

	id, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "wrote %s.key and %s.pub, key %s\n", out, out, id)

	return err
}

// writeNew writes a file that must not exist yet.
func writeNew(p string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(filepath.Clean(p), os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

// signModule signs the tree digest of the module in dir and writes the
// signature into its .rig-signatures directory, which the digest leaves
// out.
func signModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string) error {
	key, err := readPrivateKey(keyFile)
	if err != nil {
		return err
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	digest, err := m.Digest()
	if err != nil {
		return err
	}

	md := m.Metadata()

	s, err := sign.Sign(key, digest, md.Name, md.Version)
	if err != nil {
		return err
	}

	id, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	sigDir := filepath.Join(dir, sign.SignatureDir)
	if err := os.MkdirAll(sigDir, 0o750); err != nil {
		return err
	}

	p := filepath.Join(sigDir, sign.FileName(id))
	if err := os.WriteFile(p, append(data, '\n'), 0o644); err != nil { //nolint:gosec // G306: a signature is public
		return err
	}

	_, err = fmt.Fprintf(stdout, "signed %s %s (%s) with %s: %s\n", md.Name, md.Version, digest, id, p)

	return err
}

// readPrivateKey reads a private key with the password of RIG_KEY_PASSWORD.
func readPrivateKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if keyFile == "" {
		return nil, errKeyFlag
	}

	password, ok := os.LookupEnv(passwordEnv)
	if !ok {
		return nil, errPassword
	}

	text, err := os.ReadFile(filepath.Clean(keyFile))
	if err != nil {
		return nil, err
	}

	return sign.ParsePrivateKey(text, []byte(password))
}

// verifyModule verifies the signatures of the module in dir with a public
// key.
func verifyModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string) error {
	if keyFile == "" {
		return errKeyFlag
	}

	text, err := os.ReadFile(filepath.Clean(keyFile))
	if err != nil {
		return err
	}

	pub, err := sign.ParsePublicKey(text)
	if err != nil {
		return err
	}

	eng, err := offlineEngine(logger)
	if err != nil {
		return err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return err
	}

	if diags.HasErrors() {
		return reportDiags(stderr, dir, diags)
	}

	digest, err := m.Digest()
	if err != nil {
		return err
	}

	sigs, err := sign.ReadSignatures(os.DirFS(dir), ".")
	if err != nil {
		return err
	}

	if len(sign.Verify(digest, sigs, []sign.PublicKey{pub})) == 0 {
		return fmt.Errorf("%s (%s) has no signature of %s: %w", dir, digest, pub.ID, errNotSigned)
	}

	_, err = fmt.Fprintf(stdout, "%s (%s) is signed by %s\n", dir, digest, pub.ID)

	return err
}

var errNotSigned = errors.New("not signed")
