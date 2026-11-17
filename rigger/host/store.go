// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import "context"

// Store keeps the data of the engine that must outlive a run: the cache of
// modules by digest, copies of registry entries, saved plans (design 12.6).
// It never holds the record of what is installed; that is the releases of
// the cluster (design 9.3).
//
// Keys are paths separated by slashes, such as "modules/sha256/<hex>". The
// engine names them; the host maps them to files or to its own storage. The
// engine verifies what it reads back, so a Store needs no integrity checks
// of its own.
type Store interface {
	// Get returns the data of a key, or an error matching ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put writes the data of a key, replacing what it held.
	Put(ctx context.Context, key string, data []byte) error
	// Delete removes a key. A missing key is not an error.
	Delete(ctx context.Context, key string) error
}
