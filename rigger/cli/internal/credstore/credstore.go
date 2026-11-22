// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package credstore keeps the credentials of the rigger program by host
// (review of 2026-11-22, RIG-44 and RIG-46). A credential goes to the one
// host it is for, over https (http on localhost only): a source that a
// stack, a lock file or an entry of an index names is chosen by others, so
// a credential for "whoever asks" would reach them.
//
// Two places hold them: the file of rigger login, by host, for both kinds
// of sources; and the environment, RIGGER_<KIND>_USERNAME, _PASSWORD and
// _HOST, for the host it names. The file comes first.
package credstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Kind is the kind of a source: what the variables of the environment are
// named by.
type Kind string

// The kinds of sources with credentials.
const (
	OCI Kind = "OCI"
	Git Kind = "GIT"
)

// FileEnv names the file of the credentials in place of the default.
const FileEnv = "RIGGER_CREDENTIALS_FILE"

// Store is the credentials of the hosts.
type Store struct {
	path string
	env  func(string) string

	mu sync.Mutex // a login and a logout of this process, one at a time
}

// New returns the store of the file path, with the environment of env.
func New(path string, env func(string) string) *Store {
	return &Store{path: path, env: env}
}

// Default returns the store of the rigger program: the file of
// RIGGER_CREDENTIALS_FILE, else rigger/credentials.json in the
// configuration directory of the user, and the environment of the process.
func Default() (*Store, error) {
	if p := os.Getenv(FileEnv); p != "" {
		return New(p, os.Getenv), nil
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}

	return New(filepath.Join(dir, "rigger", "credentials.json"), os.Getenv), nil
}

// entry is the credentials of a host in the file.
type entry struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password"`
}

type file struct {
	Hosts map[string]entry `json:"hosts"`
}

// Lookup returns the credentials for a request to address, a URL or a host
// (then https). ok is false when the host has none. A host that has some
// and is reached over http, not on localhost, is an error: the credentials
// would cross the network as they are.
func (s *Store) Lookup(kind Kind, address string) (user, password string, ok bool, err error) {
	scheme, host, err := origin(address)
	if err != nil {
		return "", "", false, err
	}

	user, password, ok, err = s.find(kind, host)
	if err != nil || !ok {
		return "", "", false, err
	}

	if scheme != "https" && !isLocalhost(host) {
		return "", "", false, fmt.Errorf("the credentials of %s go over https only, and this address is %s", host, scheme)
	}

	return user, password, true, nil
}

// find returns the credentials of a host: of the file, else of the
// environment when it names the host.
func (s *Store) find(kind Kind, host string) (user, password string, ok bool, err error) {
	f, err := s.read()
	if err != nil {
		return "", "", false, err
	}

	if e, ok := f.Hosts[host]; ok {
		return e.Username, e.Password, true, nil
	}

	prefix := "RIGGER_" + string(kind) + "_"

	password = s.env(prefix + "PASSWORD")
	if password == "" {
		return "", "", false, nil
	}

	named := s.env(prefix + "HOST")
	if named == "" {
		return "", "", false, fmt.Errorf("%sPASSWORD is set without %sHOST: name the one host it is for", prefix, prefix)
	}

	_, bound, err := origin(named)
	if err != nil {
		return "", "", false, fmt.Errorf("%sHOST: %w", prefix, err)
	}

	if bound != host {
		return "", "", false, nil
	}

	return s.env(prefix + "USERNAME"), password, true, nil
}

// Login keeps the credentials of a host, a host or a URL of it.
func (s *Store) Login(address, user, password string) error {
	if password == "" {
		return errors.New("no password")
	}

	_, host, err := origin(address)
	if err != nil {
		return err
	}

	return s.change(func(f *file) bool {
		f.Hosts[host] = entry{Username: user, Password: password}

		return true
	})
}

// Logout forgets the credentials of a host; it reports whether it had
// some.
func (s *Store) Logout(address string) (bool, error) {
	_, host, err := origin(address)
	if err != nil {
		return false, err
	}

	var had bool

	err = s.change(func(f *file) bool {
		_, had = f.Hosts[host]
		delete(f.Hosts, host)

		return had
	})

	return had, err
}

// change writes the file as fn changes it, by a rename: a reader sees the
// old file or the new one.
func (s *Store) change(fn func(*file) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := s.read()
	if err != nil {
		return err
	}

	if !fn(f) {
		return nil
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()

		return err
	}

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), s.path)
}

func (s *Store) read() (*file, error) {
	f := &file{Hosts: map[string]entry{}}

	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}

	if f.Hosts == nil {
		f.Hosts = map[string]entry{}
	}

	return f, nil
}

// origin returns the scheme and the host of an address: a URL, or a host
// and maybe a path (then https). The host is in lower case, without the
// default port of its scheme.
func origin(address string) (scheme, host string, err error) {
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}

	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		// The error of url.Parse quotes the address, which may hold a secret.
		return "", "", errors.New("not a host or a URL of one")
	}

	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Host)

	if p := u.Port(); (scheme == "https" && p == "443") || (scheme == "http" && p == "80") {
		host = strings.ToLower(u.Hostname())
	}

	return scheme, host, nil
}

func isLocalhost(host string) bool {
	name := host
	if u, err := url.Parse("http://" + host); err == nil {
		name = u.Hostname()
	}

	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}
