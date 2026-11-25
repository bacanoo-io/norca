// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/sign"
)

// passwordEnv names the environment variable that holds the password of a
// private key.
const passwordEnv = "RIGGER_KEY_PASSWORD" //nolint:gosec // G101: the name of a variable, not a credential

var (
	errPassword = errors.New(passwordEnv + " holds the password of the private key; set it (an empty value is a key without a password)")
	errKeyFlag  = errors.New("the command needs --key")
)

// newKeyCommand is rigger key generate|sign|verify (design 8.1, 10).
func (a *app) newKeyCommand() *cobra.Command {
	return newGroup(&cobra.Command{
		Use:   "key",
		Short: "Signing keys: generate, sign, verify",
		Long: `The keys modules are signed with: a key pair, the signature of the tree
digest of a module, and its check.`,
	},
		a.newKeySubcommand("generate", "Write a new key pair",
			`Write a new key pair: <out>.key, the private key encrypted with the
password of RIGGER_KEY_PASSWORD (empty: no password), and <out>.pub. An
existing file is not overwritten.`,
			`  RIGGER_KEY_PASSWORD=secret rigger key generate --out release`),
		a.newKeySubcommand("sign [dir]", "Sign a module",
			`Sign the tree digest of the module in dir (default: the current one) with
the private key of --key (its password in RIGGER_KEY_PASSWORD), into its
.rigger-signatures directory.`,
			`  rigger key sign --key release.key
  rigger key sign modules/web --key release.key`),
		a.newKeySubcommand("verify [dir]", "Check that a module is signed by a key",
			`Check that the module in dir (default: the current one) is signed by the
public key of --key.`,
			`  rigger key verify --key release.pub`),
	)
}

// newKeySubcommand is one command of rigger key. Each parses the flags they
// all took before and shows its own.
func (a *app) newKeySubcommand(use, short, long, example string) *cobra.Command {
	var key, out string

	name, _, _ := strings.Cut(use, " ")

	cmd := &cobra.Command{
		Use: use, Short: short, Long: long, Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) > 0 {
				dir = args[0]
			}

			switch name {
			case "generate":
				return generateKey(a.stdout, out)
			case "sign":
				return signModule(cmd.Context(), a.logger, a.stdout, a.stderr, dir, key)
			default:
				return verifyModule(cmd.Context(), a.logger, a.stdout, a.stderr, dir, key)
			}
		},
	}

	cmd.Flags().StringVar(&key, keyFlag, "", "the private key (sign) or the public key (verify)")
	cmd.Flags().StringVar(&out, "out", "rigger", "the files to write, `<out>`.key and <out>.pub")

	if name == "generate" {
		showOnly(cmd, "out")
	} else {
		showOnly(cmd, keyFlag)
	}

	return cmd
}

// generateKey writes a new key pair: <out>.key encrypted with the password
// of RIGGER_KEY_PASSWORD, and <out>.pub.
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

	id, err := sign.KeyID(&key.PublicKey)
	if err != nil {
		return err
	}

	if err := writeNew(out+".key", private, 0o600); err != nil {
		return err
	}

	if err := writeNew(out+".pub", public, 0o644); err != nil {
		// The pair is written whole or not at all: a private key without
		// its public key is of no use, and its file would refuse the name
		// to the next run.
		_ = os.Remove(filepath.Clean(out + ".key"))

		return err
	}

	_, err = fmt.Fprintf(w, "wrote %s.key and %s.pub, key %s\n", out, out, id)

	return err
}

// writeNew writes a file that must not exist yet. A file it could not
// write whole is removed.
func writeNew(p string, data []byte, perm os.FileMode) error {
	p = filepath.Clean(p)

	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		_ = os.Remove(p)
	}

	return err
}

// signed is what a signature names: a module, or a published stack.
type signed struct {
	what, name, version, digest string
}

// What a signature names.
const (
	whatModule = "module"
	whatStack  = "stack"
)

// readSigned reads the module or the published stack (a directory with
// stack.hcl) in dir and its tree digest.
func readSigned(ctx context.Context, logger *slog.Logger, stderr io.Writer, dir string) (signed, error) {
	eng, err := offlineEngine(logger)
	if err != nil {
		return signed{}, err
	}

	if _, err := os.Stat(filepath.Join(dir, stackFile)); err == nil {
		root, rel, err := moduleRoot(dir)
		if err != nil {
			return signed{}, err
		}

		s, diags := eng.ReadStack(ctx, os.DirFS(root), rel)
		if diags.HasErrors() {
			return signed{}, reportDiags(stderr, dir, diags)
		}

		md := s.Metadata()
		if md == nil {
			return signed{}, errUnpublished
		}

		digest, err := s.Digest()

		return signed{what: whatStack, name: md.Name, version: md.Version, digest: digest}, err
	}

	m, diags, err := loadModule(ctx, eng, dir)
	if err != nil {
		return signed{}, err
	}

	if diags.HasErrors() {
		return signed{}, reportDiags(stderr, dir, diags)
	}

	digest, err := m.Digest()
	md := m.Metadata()

	return signed{what: whatModule, name: md.Name, version: md.Version, digest: digest}, err
}

// stackFile is the file that makes a directory a stack.
const stackFile = "stack.hcl"

var errUnpublished = errors.New("a stack is signed when it is published: it needs metadata.hcl with its stack block (design 7.5)")

// signModule signs the tree digest of the module or published stack in dir
// and writes the signature into its .rigger-signatures directory, which the
// digest leaves out.
func signModule(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, dir, keyFile string) error {
	key, err := readPrivateKey(keyFile)
	if err != nil {
		return err
	}

	t, err := readSigned(ctx, logger, stderr, dir)
	if err != nil {
		return err
	}

	file, id, err := signatureFile(dir, key)
	if err != nil {
		return err
	}

	if err := writeSignature(file, key, t); err != nil {
		return err
	}

	return reportSigned(stdout, t, id, file)
}

// signatureFile returns the file of the signature of a key in the
// .rigger-signatures directory of dir, and the ID of the key.
func signatureFile(dir string, key *ecdsa.PrivateKey) (file, id string, err error) {
	id, err = sign.KeyID(&key.PublicKey)
	if err != nil {
		return "", "", err
	}

	return filepath.Join(dir, sign.SignatureDir, sign.FileName(id)), id, nil
}

// writeSignature signs the tree digest of t with key and writes the
// signature to file, in a .rigger-signatures directory.
func writeSignature(file string, key *ecdsa.PrivateKey, t signed) error {
	s, err := sign.Sign(key, t.digest, t.name, t.version)
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}

	return os.WriteFile(file, append(data, '\n'), 0o644) //nolint:gosec // G306: a signature is public
}

// reportSigned says that t was signed by the key of id into file.
func reportSigned(stdout io.Writer, t signed, id, file string) error {
	_, err := fmt.Fprintf(stdout, "signed %s %s (%s) with %s: %s\n", t.name, t.version, t.digest, id, file)

	return err
}

// readPrivateKey reads a private key with the password of RIGGER_KEY_PASSWORD.
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

	t, err := readSigned(ctx, logger, stderr, dir)
	if err != nil {
		return err
	}

	digest := t.digest

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
