// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/bacanoo-io/norca/rigger/cli/internal/credstore"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// stdin is where rigger login reads a password; tests replace it.
var stdin io.Reader = os.Stdin

// maxPasswordBytes bounds a password read from standard input.
const maxPasswordBytes = 64 << 10

// runLogin runs rigger login <host> [-username name]: the password is the
// first line of standard input.
func runLogin(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	host, args := hostArg(args)

	flags := flag.NewFlagSet("rigger login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	user := flags.String("username", "", "the user name (none for a token)")

	if err := flags.Parse(args); err != nil {
		return exitError
	}

	return run(ctx, logger, stderr, cmdLogin, func(context.Context) error {
		if host == "" {
			return errors.New("login needs a host: rigger login <host> [-username name], the password on standard input")
		}

		password, err := readPassword(stdin)
		if err != nil {
			return err
		}

		store, err := credstore.Default()
		if err != nil {
			return err
		}

		if err := store.Login(host, *user, password); err != nil {
			return err
		}

		_, err = fmt.Fprintf(stdout, "logged in to %s\n", host)

		return err
	})
}

// runLogout runs rigger logout <host>.
func runLogout(ctx context.Context, logger *slog.Logger, stdout, stderr io.Writer, args []string) int {
	host, _ := hostArg(args)

	return run(ctx, logger, stderr, cmdLogout, func(context.Context) error {
		if host == "" {
			return errors.New("logout needs a host: rigger logout <host>")
		}

		store, err := credstore.Default()
		if err != nil {
			return err
		}

		had, err := store.Logout(host)
		if err != nil {
			return err
		}

		if !had {
			return fmt.Errorf("%s has no credentials of rigger login", host)
		}

		_, err = fmt.Fprintf(stdout, "logged out of %s\n", host)

		return err
	})
}

// hostArg takes the host before the flags.
func hostArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}

	return "", args
}

// readPassword reads the first line of r.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, maxPasswordBytes)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}

	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("no password: give it on standard input (echo \"$TOKEN\" | rigger login <host>)")
	}

	return password, nil
}

// credentials are the credentials of the rigger program for the sources of
// a kind, by host (credstore): those of rigger login, else those of the
// environment for the one host it names.
type credentials struct{ kind credstore.Kind }

// lookup returns the user and the password for address, ok false for none.
func (c credentials) lookup(address string) (user, password string, ok bool, err error) {
	store, err := credstore.Default()
	if err != nil {
		return "", "", false, err
	}

	return store.Lookup(c.kind, address)
}

// oci answers the OCI client, which asks by the host of a registry.
func (c credentials) oci(host string) (user, password string, ok bool, err error) {
	return c.lookup(host)
}

// gitCredentials are the credentials of the rigger program for git
// repositories (the fetcher reads them with go-git, not with the git
// program, so the credential helpers and ~/.ssh/config of git do not
// apply): an ssh URL and user@host:path authenticate with the keys of the
// ssh agent and the known hosts of the user; an https repository with the
// credentials of its host (rigger login, else RIGGER_GIT_USERNAME and
// RIGGER_GIT_PASSWORD for the host of RIGGER_GIT_HOST), else anonymously.
func gitCredentials(_ context.Context, repository string) (*fetch.Credentials, error) {
	if fetch.SSHRepository(repository) {
		return &fetch.Credentials{SSHAgent: true}, nil
	}

	user, password, ok, err := credentials{kind: credstore.Git}.lookup(fetch.RepositoryURL(repository))
	if err != nil || !ok {
		return nil, err
	}

	return &fetch.Credentials{Username: user, Password: password}, nil
}
