// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package host

import (
	"context"
	"errors"
	"log/slog"
	"maps"
)

// redacted is what a Sensitive value shows instead of itself.
const redacted = "(sensitive)"

// ErrSensitive is returned when a Sensitive value is encoded as JSON or
// text.
var ErrSensitive = errors.New("host: a sensitive value is not encoded")

// Sensitive is a value that must not appear in a plan, a log or a revision
// (design 6.3). Printed with the fmt package or logged with slog it shows
// "(sensitive)"; encoded as JSON or text it fails with ErrSensitive. Only
// Reveal gives the value, where it is handed to the secret store.
//
// The zero value is an empty sensitive value.
type Sensitive struct {
	value string
}

// NewSensitive wraps a value that must not be shown.
func NewSensitive(value string) Sensitive {
	return Sensitive{value: value}
}

// Reveal returns the value. Call it only where the value goes to the secret
// store or to the workload, never for output.
func (s Sensitive) Reveal() string {
	return s.value
}

// String implements fmt.Stringer: it never shows the value.
func (Sensitive) String() string {
	return redacted
}

// GoString implements fmt.GoStringer, for %#v: it never shows the value.
func (Sensitive) GoString() string {
	return redacted
}

// LogValue implements slog.LogValuer: it never shows the value.
func (Sensitive) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// MarshalJSON refuses to encode the value.
func (Sensitive) MarshalJSON() ([]byte, error) {
	return nil, ErrSensitive
}

// MarshalText refuses to encode the value.
func (Sensitive) MarshalText() ([]byte, error) {
	return nil, ErrSensitive
}

// Secret is a secret a stack creates once (design 6.3, 12.6): the
// generated passwords and keys of a stack, and the secrets of its secret
// blocks.
type Secret struct {
	// Path is the path of the secret in the secret store.
	Path string
	// Items are the items of the secret.
	Items map[string]Sensitive
	// Overwrite replaces the items when the secret exists. Without it an
	// existing secret is left as it is.
	Overwrite bool
	// Replaces, with Overwrite, are the items the caller read: the write
	// replaces only those, as a check and set. A secret that has others by
	// then stays, and Ensure reports that it wrote nothing.
	Replaces map[string]Sensitive
}

// ReplacesOnly reports whether a write of the secret may replace the items
// the store has now (Replaces).
func (s Secret) ReplacesOnly(now map[string]Sensitive) bool {
	return maps.EqualFunc(s.Replaces, now, func(a, b Sensitive) bool { return a.Reveal() == b.Reveal() })
}

// Secrets is the secret store of the cluster.
type Secrets interface {
	// Ensure creates the secret unless it exists, or replaces its items
	// with Overwrite. It reports whether it wrote.
	Ensure(ctx context.Context, secret Secret) (written bool, err error)
	// Read returns the items of a secret, or an error matching ErrNotFound.
	// The engine reads back a generated value whose local copy is lost
	// (design 12.6).
	Read(ctx context.Context, path string) (map[string]Sensitive, error)
}
