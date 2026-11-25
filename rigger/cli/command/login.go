// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/rigger/cli/internal/credstore"
	"github.com/bacanoo-io/norca/rigger/fetch"
)

// stdin is where rigger login reads a password; tests replace it.
var stdin io.Reader = os.Stdin

// maxPasswordBytes bounds a password read from standard input.
const maxPasswordBytes = 64 << 10

// newLoginCommand is rigger login <host> [--username name]: the password is
// the first line of standard input.
func (a *app) newLoginCommand() *cobra.Command {
	var user string

	cmd := &cobra.Command{
		Use:   "login <host>",
		Short: "Keep the credentials of a host of git or OCI sources",
		Long: `Keep the credentials of a host of git or OCI sources, for fetching modules
from it. The password, or a token, is the first line of standard input.
A credential goes to its host only, over https. Without one of login the
variables RIGGER_GIT_* and RIGGER_OCI_* apply (rigger help environment).`,
		Example: `  echo "$TOKEN" | rigger login git.example.com
  rigger login registry.example.com --username ci < password.txt`,
		RunE: func(_ *cobra.Command, args []string) error {
			host := firstArg(args)
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

			if err := store.Login(host, user, password); err != nil {
				return err
			}

			_, err = fmt.Fprintf(a.stdout, "logged in to %s\n", host)

			return err
		},
	}

	cmd.Flags().StringVar(&user, "username", "", "the user `name` (none for a token)")

	return cmd
}

// newLogoutCommand is rigger logout <host>.
func (a *app) newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "logout <host>",
		Short:   "Forget the credentials of a host",
		Long:    `Forget the credentials rigger login keeps for a host.`,
		Example: `  rigger logout git.example.com`,
		RunE: func(_ *cobra.Command, args []string) error {
			host := firstArg(args)
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

			_, err = fmt.Fprintf(a.stdout, "logged out of %s\n", host)

			return err
		},
	}
}

// firstArg is the first argument, the host of login and logout; empty for
// none.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}

	return args[0]
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
