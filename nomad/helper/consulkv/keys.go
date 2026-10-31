// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package consulkv maps the keys of the Consul KV of the Consul-compatible
// API to the keys of a NATS JetStream key-value bucket, and holds the export
// format of the bucket in the backups.
package consulkv

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The keys of the Consul KV are mapped to NATS keys: the segments ("/")
// become the tokens of the subject ("."), every byte but letters, digits,
// "-" and "_" becomes "=XX" (its hexadecimal value) and an empty segment
// becomes "=", so every key has a valid NATS key and the keys below a
// directory are matched by "<directory>.>".

// EncodeKey returns the NATS key of a Consul key.
func EncodeKey(key string) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		segments[i] = encodeToken(seg)
	}
	return strings.Join(segments, ".")
}

// encodeToken returns the NATS token of a segment; a "/" in it is escaped.
func encodeToken(seg string) string {
	if seg == "" {
		return "="
	}
	var b strings.Builder
	for j := 0; j < len(seg); j++ {
		c := seg[j]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "=%02X", c)
		}
	}
	return b.String()
}

// DecodeKey returns the Consul key of a NATS key.
func DecodeKey(k string) (string, error) {
	tokens := strings.Split(k, ".")
	for i, tok := range tokens {
		seg, err := decodeToken(tok)
		if err != nil {
			return "", fmt.Errorf("invalid escape in %q", k)
		}
		tokens[i] = seg
	}
	return strings.Join(tokens, "/"), nil
}

// decodeToken returns the segment of a NATS token.
func decodeToken(tok string) (string, error) {
	if tok == "=" {
		return "", nil
	}
	var b strings.Builder
	for j := 0; j < len(tok); j++ {
		if tok[j] != '=' {
			b.WriteByte(tok[j])
			continue
		}
		if j+2 >= len(tok) {
			return "", errors.New("invalid escape")
		}
		v, err := strconv.ParseUint(tok[j+1:j+3], 16, 8)
		if err != nil {
			return "", errors.New("invalid escape")
		}
		b.WriteByte(byte(v))
		j += 2
	}
	return b.String(), nil
}

// The bucket of the system bus keeps the keys of every namespace and
// Variables path: a NATS key there is "<namespace>.<path>.<key>", the
// namespace and the Variables path of the keys (consul/kv by default) one
// token each, then the NATS key of the Consul key. The keys of clients whose
// kv blocks name another namespace or path are separate, as with the
// variables backend, and the Variables rules of a namespace and path apply
// to exactly the keys kept under them.
//
// white room bugfix: without the namespace and the path in the key, a token
// with Variables write on any path of any namespace could write every key of
// the bucket by naming that path and namespace in its KV request.

// ScopePrefix returns the beginning of the NATS keys of the bucket of the
// system bus for the keys under path in namespace, with the trailing dot.
func ScopePrefix(namespace, path string) string {
	return encodeToken(namespace) + "." + encodeToken(path) + "."
}

// SplitScopedKey returns the namespace, the Variables path and the Consul
// key of a NATS key of the bucket of the system bus.
func SplitScopedKey(k string) (namespace, path, key string, err error) {
	parts := strings.SplitN(k, ".", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("%q is not a key of the Consul KV of the system bus", k)
	}
	if namespace, err = decodeToken(parts[0]); err != nil {
		return "", "", "", fmt.Errorf("invalid escape in %q", k)
	}
	if path, err = decodeToken(parts[1]); err != nil {
		return "", "", "", fmt.Errorf("invalid escape in %q", k)
	}
	if key, err = DecodeKey(parts[2]); err != nil {
		return "", "", "", err
	}
	return namespace, path, key, nil
}
