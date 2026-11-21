// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package host holds the ports of Rigger: the interfaces a host program
// implements so that the engine reaches the cluster, the release API, the
// secret store and the storage of the host (design, section 12.2).
//
// The rigger program implements them over the HTTP API of Norca and the disk of
// the machine it runs on; the Norca server implements them over its own
// state. The engine imports nothing of Norca: what it needs passes through
// these interfaces as plain data.
//
// Every method takes a context and stops when it is cancelled.
package host

import "errors"

// ErrNotFound is matched (with errors.Is) by the error a port returns when
// the thing asked for does not exist: a release, a secret, a key of the
// store.
var ErrNotFound = errors.New("host: not found")
