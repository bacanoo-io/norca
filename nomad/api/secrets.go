// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrSecretNotFound is returned when reading a secret that does not exist.
var ErrSecretNotFound = errors.New("secret not found")

// Secrets is used to access the secrets of the built-in secret store.
type Secrets struct {
	client *Client
}

// Secrets returns a new handle on the secrets.
func (c *Client) Secrets() *Secrets {
	return &Secrets{client: c}
}

// SecretMeta is the user settable metadata of a secret. It is never secret.
type SecretMeta struct {
	Description string            `json:",omitempty"`
	Owner       string            `json:",omitempty"`
	Labels      map[string]string `json:",omitempty"`
	ExpiresAt   *time.Time        `json:",omitempty"`
	RotateEvery time.Duration     `json:",omitempty"`

	// MaxVersions is the number of versions kept (default 10).
	MaxVersions int `json:",omitempty"`
}

// SecretVersion describes a retained version of a secret, never its value.
type SecretVersion struct {
	Version    uint64
	CreateTime int64
	Deleted    bool
	Destroyed  bool
	ValueHash  string
}

// Secret is the metadata of a secret: everything except its values.
type Secret struct {
	Namespace string
	Path      string

	SecretMeta

	CurrentVersion uint64
	Deleted        bool
	Destroyed      bool
	ItemKeys       []string
	ValueHash      string
	Versions       []*SecretVersion

	CreateIndex uint64
	ModifyIndex uint64
	CreateTime  int64
	ModifyTime  int64
}

// SecretStub is the list form of a secret.
type SecretStub struct {
	Namespace      string
	Path           string
	Description    string
	Owner          string
	Labels         map[string]string
	ExpiresAt      *time.Time
	RotateEvery    time.Duration
	CurrentVersion uint64
	Deleted        bool
	Destroyed      bool
	ValueHash      string
	CreateIndex    uint64
	ModifyIndex    uint64
	CreateTime     int64
	ModifyTime     int64
}

// SecretValue is the value of a version of a secret, with the secret's
// metadata.
type SecretValue struct {
	Secret  *Secret
	Version uint64
	Items   map[string]string
}

// SecretWrite is the body of a write: the items of a new version (none for
// a metadata-only update) and the metadata (nil keeps the existing metadata
// of the secret).
type SecretWrite struct {
	Items map[string]string `json:",omitempty"`
	Meta  *SecretMeta       `json:",omitempty"`
}

// secretWriteResult is the response of a write.
type secretWriteResult struct {
	Secret *Secret
}

// secretEndpoint returns the URL path of a secret.
func secretEndpoint(path string) string {
	return "/v1/secret/" + strings.Trim(path, "/")
}

// List lists the metadata of the secrets of the namespace of the query
// options whose path starts with prefix.
func (s *Secrets) List(prefix string, q *QueryOptions) ([]*SecretStub, *QueryMeta, error) {
	if q == nil {
		q = &QueryOptions{}
	}
	qc := *q
	if prefix != "" {
		qc.Prefix = prefix
	}
	var out []*SecretStub
	qm, err := s.client.query("/v1/secrets", &out, &qc)
	if err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Info reads the metadata of a secret, never its value.
func (s *Secrets) Info(path string, q *QueryOptions) (*Secret, *QueryMeta, error) {
	var out Secret
	qm, err := s.client.query(secretEndpoint(path)+"?metadata", &out, q)
	if err != nil {
		return nil, nil, secretNotFound(err)
	}
	return &out, qm, nil
}

// Read reads the value of a version of a secret; version 0 reads the
// current version. The read is audited.
func (s *Secrets) Read(path string, version uint64, q *QueryOptions) (*SecretValue, *QueryMeta, error) {
	endpoint := secretEndpoint(path)
	if version > 0 {
		endpoint += "?version=" + strconv.FormatUint(version, 10)
	}
	var out SecretValue
	qm, err := s.client.query(endpoint, &out, q)
	if err != nil {
		return nil, nil, secretNotFound(err)
	}
	return &out, qm, nil
}

// Write writes a new version of a secret and/or its metadata.
func (s *Secrets) Write(path string, w *SecretWrite, q *WriteOptions) (*Secret, *WriteMeta, error) {
	return s.write(secretEndpoint(path), w, q)
}

// CheckedWrite writes a secret if its modify index is checkIndex; 0 creates
// a secret that must not exist yet. A mismatch fails with a 409 error.
func (s *Secrets) CheckedWrite(path string, checkIndex uint64, w *SecretWrite, q *WriteOptions) (*Secret, *WriteMeta, error) {
	return s.write(secretEndpoint(path)+"?cas="+strconv.FormatUint(checkIndex, 10), w, q)
}

func (s *Secrets) write(endpoint string, w *SecretWrite, q *WriteOptions) (*Secret, *WriteMeta, error) {
	var out secretWriteResult
	wm, err := s.client.put(endpoint, w, &out, q)
	if err != nil {
		return nil, nil, err
	}
	return out.Secret, wm, nil
}

// SecretVersionsOptions select the versions of a delete, undelete or
// destroy, and make it conditional.
type SecretVersionsOptions struct {
	// Versions are the targeted versions; none targets the current version
	// (delete and undelete) or the whole secret (destroy).
	Versions []uint64

	// CheckIndex, if set, applies the operation only if the secret's modify
	// index is the given one.
	CheckIndex *uint64
}

func (o *SecretVersionsOptions) query() string {
	if o == nil {
		return ""
	}
	v := url.Values{}
	if len(o.Versions) > 0 {
		parts := make([]string, len(o.Versions))
		for i, version := range o.Versions {
			parts[i] = strconv.FormatUint(version, 10)
		}
		v.Set("versions", strings.Join(parts, ","))
	}
	if o.CheckIndex != nil {
		v.Set("cas", strconv.FormatUint(*o.CheckIndex, 10))
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// Delete soft deletes versions of a secret; without versions the current
// version.
func (s *Secrets) Delete(path string, opts *SecretVersionsOptions, q *WriteOptions) (*Secret, *WriteMeta, error) {
	var out secretWriteResult
	wm, err := s.client.delete(secretEndpoint(path)+opts.query(), nil, &out, q)
	if err != nil {
		return nil, nil, secretNotFound(err)
	}
	return out.Secret, wm, nil
}

// Undelete restores soft deleted versions of a secret; without versions the
// current version.
func (s *Secrets) Undelete(path string, opts *SecretVersionsOptions, q *WriteOptions) (*Secret, *WriteMeta, error) {
	var out secretWriteResult
	wm, err := s.client.post(secretEndpoint(path)+"/undelete"+opts.query(), nil, &out, q)
	if err != nil {
		return nil, nil, secretNotFound(err)
	}
	return out.Secret, wm, nil
}

// Destroy destroys versions of a secret permanently; without versions the
// whole secret with all of its versions, and the returned secret is nil.
func (s *Secrets) Destroy(path string, opts *SecretVersionsOptions, q *WriteOptions) (*Secret, *WriteMeta, error) {
	var out secretWriteResult
	wm, err := s.client.post(secretEndpoint(path)+"/destroy"+opts.query(), nil, &out, q)
	if err != nil {
		return nil, nil, secretNotFound(err)
	}
	return out.Secret, wm, nil
}

// secretNotFound turns a 404 response for a missing secret into
// ErrSecretNotFound; other errors are returned unchanged.
func secretNotFound(err error) error {
	var ure UnexpectedResponseError
	if errors.As(err, &ure) && ure.StatusCode() == http.StatusNotFound &&
		strings.Contains(ure.Body(), "secret not found") {
		return fmt.Errorf("%w", ErrSecretNotFound)
	}
	return err
}
