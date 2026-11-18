// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/bacanoo-io/norca/norca/api"

	"github.com/bacanoo-io/norca-rig/host"
)

// Secrets is the secret store of Norca (host.Secrets) in one namespace.
type Secrets struct {
	client    *api.Client
	namespace string
}

var _ host.Secrets = (*Secrets)(nil)

// NewSecrets returns the secret store of the namespace.
func NewSecrets(client *api.Client, namespace string) *Secrets {
	return &Secrets{client: client, namespace: namespace}
}

// Ensure creates the secret unless it exists, or replaces its items with
// Overwrite. A secret made meanwhile by another is left as it is.
func (s *Secrets) Ensure(ctx context.Context, secret host.Secret) (bool, error) {
	items := make(map[string]string, len(secret.Items))
	for k, v := range secret.Items {
		items[k] = v.Reveal()
	}

	w := &api.SecretWrite{Items: items}

	if secret.Overwrite {
		_, _, err := s.client.Secrets().Write(secret.Path, w, write(ctx, s.namespace))

		return err == nil, err
	}

	_, _, err := s.client.Secrets().CheckedWrite(secret.Path, 0, w, write(ctx, s.namespace))

	var resp api.UnexpectedResponseError
	if errors.As(err, &resp) && resp.StatusCode() == http.StatusConflict {
		return false, nil
	}

	return err == nil, err
}

// Read returns the items of the current version of a secret.
func (s *Secrets) Read(ctx context.Context, path string) (map[string]host.Sensitive, error) {
	v, _, err := s.client.Secrets().Read(path, 0, query(ctx, s.namespace))
	if err != nil {
		if errors.Is(err, api.ErrSecretNotFound) {
			return nil, errors.Join(host.ErrNotFound, err)
		}

		return nil, notFound(err)
	}

	out := make(map[string]host.Sensitive, len(v.Items))
	for k, item := range v.Items {
		out[k] = host.NewSensitive(item)
	}

	return out, nil
}
