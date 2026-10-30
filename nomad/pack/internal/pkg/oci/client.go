// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package oci stores packs as OCI artifacts in OCI registries (and OCI image
// layouts), and stores their cosign compatible signatures next to them.
package oci

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/hashicorp/nomad/pack/internal/pkg/version"
)

// Scheme is the URL scheme of OCI registry sources.
const Scheme = "oci://"

const (
	// EnvRegistryUsername and EnvRegistryPassword hold credentials for the
	// OCI registry a command talks to. They take precedence over the docker
	// configuration but not over command line flags.
	EnvRegistryUsername = "NORCA_PACK_REGISTRY_USERNAME"
	EnvRegistryPassword = "NORCA_PACK_REGISTRY_PASSWORD"
)

// IsOCI reports whether source is an OCI registry URL.
func IsOCI(source string) bool {
	return strings.HasPrefix(source, Scheme)
}

// Location is an OCI registry location: a registry host and a repository
// path, optionally with a tag or digest.
type Location struct {
	// Host is the registry host, with an optional port.
	Host string
	// Repository is the repository path, e.g. "packs/redis".
	Repository string
	// Reference is a tag or digest, or empty.
	Reference string
}

// ParseLocation parses "oci://host[:port]/path[:tag|@digest]". The path may
// be empty for a registry root.
func ParseLocation(s string) (Location, error) {
	if !IsOCI(s) {
		return Location{}, fmt.Errorf("%q is not an OCI location, it must start with %s", s, Scheme)
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(s, Scheme), "/")
	if rest == "" {
		return Location{}, fmt.Errorf("%q has no registry host", s)
	}
	if !strings.Contains(rest, "/") {
		ref := registry.Reference{Registry: rest}
		if err := ref.ValidateRegistry(); err != nil {
			return Location{}, err
		}
		return Location{Host: rest}, nil
	}
	ref, err := registry.ParseReference(rest)
	if err != nil {
		return Location{}, err
	}
	return Location{Host: ref.Registry, Repository: ref.Repository, Reference: ref.Reference}, nil
}

// Child returns the location of the repository below l named name.
func (l Location) Child(name string) Location {
	repo := name
	if l.Repository != "" {
		repo = l.Repository + "/" + name
	}
	return Location{Host: l.Host, Repository: repo}
}

// WithReference returns l with the given tag or digest.
func (l Location) WithReference(ref string) Location {
	l.Reference = ref
	return l
}

// RepositoryName returns "host/repository", the name cosign uses as the
// identity of signed artifacts.
func (l Location) RepositoryName() string {
	if l.Repository == "" {
		return l.Host
	}
	return l.Host + "/" + l.Repository
}

// String returns the location as oci:// URL.
func (l Location) String() string {
	s := Scheme + l.RepositoryName()
	switch {
	case l.Reference == "":
	case strings.Contains(l.Reference, ":"):
		s += "@" + l.Reference
	default:
		s += ":" + l.Reference
	}
	return s
}

// Options configure the connection to an OCI registry.
type Options struct {
	// PlainHTTP uses HTTP instead of HTTPS, for internal registries without
	// TLS.
	PlainHTTP bool
	// CACertFile is a PEM file with additional CA certificates to trust.
	CACertFile string
	// TLSSkipVerify disables the verification of the registry certificate.
	TLSSkipVerify bool
	// Username and Password are static credentials. If empty, the
	// credentials of the environment and then of the docker configuration
	// (including credential helpers) are used.
	Username string
	Password string
	// NoDockerConfig disables reading credentials from the docker
	// configuration.
	NoDockerConfig bool
	// NoEnvCredentials disables the credentials of the environment
	// (NORCA_PACK_REGISTRY_USERNAME, ..._PASSWORD). The servers set it and
	// NoDockerConfig: they never use credentials of their own environment.
	NoEnvCredentials bool
	// AuthHost restricts the credentials to one registry host ("host" or
	// "host:port"): other hosts, such as a token service elsewhere, get no
	// credentials and no Authorization header.
	AuthHost string
}

// authHostTransport drops the Authorization header of requests to hosts
// other than host.
type authHostTransport struct {
	host string
	next http.RoundTripper
}

func (t *authHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") != "" && !sameHost(req.URL.Host, t.host) {
		req = req.Clone(req.Context())
		req.Header.Del("Authorization")
	}
	return t.next.RoundTrip(req)
}

// sameHost compares hosts with the default HTTPS port made explicit.
func sameHost(a, b string) bool {
	norm := func(h string) string {
		h = strings.ToLower(h)
		if !strings.Contains(h, ":") {
			h += ":443"
		}
		return h
	}
	return norm(a) == norm(b)
}

func (o *Options) httpClient() (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CACertFile != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pemBytes, err := os.ReadFile(o.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read registry CA certificate: %w", err)
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no certificates found in %s", o.CACertFile)
		}
		tlsConfig.RootCAs = pool
	}
	if o.TLSSkipVerify {
		tlsConfig.InsecureSkipVerify = true
	}
	transport.TLSClientConfig = tlsConfig
	var rt http.RoundTripper = transport
	if o.AuthHost != "" {
		rt = &authHostTransport{host: o.AuthHost, next: transport}
	}
	return &http.Client{Transport: retry.NewTransport(rt)}, nil
}

func (o *Options) credential() auth.CredentialFunc {
	user, pass := o.Username, o.Password
	if user == "" && pass == "" && !o.NoEnvCredentials {
		user, pass = os.Getenv(EnvRegistryUsername), os.Getenv(EnvRegistryPassword)
	}
	if user != "" || pass != "" {
		cred := auth.Credential{Username: user, Password: pass}
		if user == "" {
			// A password without a user name is used as an identity token.
			cred = auth.Credential{RefreshToken: pass}
		}
		return func(_ context.Context, hostport string) (auth.Credential, error) {
			if o.AuthHost != "" && !sameHost(hostport, o.AuthHost) {
				return auth.EmptyCredential, nil
			}
			return cred, nil
		}
	}
	if o.NoDockerConfig {
		return nil
	}
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil
	}
	return credentials.Credential(store)
}

func (o *Options) authClient() (*auth.Client, error) {
	hc, err := o.httpClient()
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("User-Agent", "norca-pack/"+version.HumanVersion())
	return &auth.Client{
		Client:     hc,
		Header:     header,
		Cache:      auth.NewCache(),
		Credential: o.credential(),
	}, nil
}

// Repository returns a client for the repository of loc.
func (o *Options) Repository(loc Location) (*remote.Repository, error) {
	if loc.Repository == "" {
		return nil, errors.New("an OCI repository path is required")
	}
	repo, err := remote.NewRepository(loc.RepositoryName())
	if err != nil {
		return nil, err
	}
	client, err := o.authClient()
	if err != nil {
		return nil, err
	}
	repo.Client = client
	repo.PlainHTTP = o.PlainHTTP
	return repo, nil
}

// Registry returns a client for the registry host of loc.
func (o *Options) Registry(loc Location) (*remote.Registry, error) {
	reg, err := remote.NewRegistry(loc.Host)
	if err != nil {
		return nil, err
	}
	client, err := o.authClient()
	if err != nil {
		return nil, err
	}
	reg.Client = client
	reg.PlainHTTP = o.PlainHTTP
	return reg, nil
}
