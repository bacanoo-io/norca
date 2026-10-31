// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"fmt"
	"strconv"
	"strings"
)

// EncodeVariablesKey returns the Variables path segment of a Consul key:
// "/" stays, every byte but letters, digits, "-" and "_" becomes "~XX" and an
// empty segment "~" (except the last one of a prefix).
func EncodeVariablesKey(key string, prefix bool) string {
	segments := strings.Split(key, "/")
	for i, seg := range segments {
		if seg == "" && !(prefix && i == len(segments)-1) {
			segments[i] = "~"
			continue
		}
		var b strings.Builder
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(&b, "~%02X", c)
			}
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, "/")
}

// DecodeVariablesKey reverses EncodeVariablesKey.
func DecodeVariablesKey(path string) (string, error) {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if seg == "~" {
			segments[i] = ""
			continue
		}
		var b strings.Builder
		for j := 0; j < len(seg); j++ {
			if seg[j] != '~' {
				b.WriteByte(seg[j])
				continue
			}
			if j+2 >= len(seg) {
				return "", fmt.Errorf("invalid escape in %q", path)
			}
			v, err := strconv.ParseUint(seg[j+1:j+3], 16, 8)
			if err != nil {
				return "", fmt.Errorf("invalid escape in %q", path)
			}
			b.WriteByte(byte(v))
			j += 2
		}
		segments[i] = b.String()
	}
	return strings.Join(segments, "/"), nil
}

// DefaultPath is the Variables path the keys of the Consul KV live under,
// and the base of their ACL rules.
const DefaultPath = "consul/kv"

// ACLPath is the Variables path whose ACL rules apply to a key of the
// Consul KV under base.
func ACLPath(base, key string) string {
	return base + "/" + EncodeVariablesKey(key, false)
}
