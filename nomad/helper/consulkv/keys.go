// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package consulkv maps the keys of the Consul KV of the Consul-compatible
// API to the keys of a NATS JetStream key-value bucket, and holds the export
// format of the bucket in the backups.
package consulkv

import (
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
		if seg == "" {
			segments[i] = "="
			continue
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
		segments[i] = b.String()
	}
	return strings.Join(segments, ".")
}

// DecodeKey returns the Consul key of a NATS key.
func DecodeKey(k string) (string, error) {
	tokens := strings.Split(k, ".")
	for i, tok := range tokens {
		if tok == "=" {
			tokens[i] = ""
			continue
		}
		var b strings.Builder
		for j := 0; j < len(tok); j++ {
			if tok[j] != '=' {
				b.WriteByte(tok[j])
				continue
			}
			if j+2 >= len(tok) {
				return "", fmt.Errorf("invalid escape in %q", k)
			}
			v, err := strconv.ParseUint(tok[j+1:j+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("invalid escape in %q", k)
			}
			b.WriteByte(byte(v))
			j += 2
		}
		tokens[i] = b.String()
	}
	return strings.Join(tokens, "/"), nil
}
