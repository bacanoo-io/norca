// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulkv

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
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

// Items of the Variable of a key (the variables backend of the KV).
const (
	ItemValue       = "value"        // a UTF-8 value
	ItemValueBase64 = "value_base64" // any other value, base64
	ItemFlags       = "flags"        // the flags, when not 0
)

// EncodeVariableItems returns the Variable items of a value and its flags.
func EncodeVariableItems(value []byte, flags uint64) map[string]string {
	items := map[string]string{}
	if utf8.Valid(value) {
		items[ItemValue] = string(value)
	} else {
		items[ItemValueBase64] = base64.StdEncoding.EncodeToString(value)
	}
	if flags != 0 {
		items[ItemFlags] = strconv.FormatUint(flags, 10)
	}
	return items
}

// DecodeVariableItems reverses EncodeVariableItems.
func DecodeVariableItems(items map[string]string) ([]byte, uint64, error) {
	var value []byte
	if raw, ok := items[ItemValueBase64]; ok {
		v, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, 0, err
		}
		value = v
	} else if raw, ok := items[ItemValue]; ok {
		value = []byte(raw)
	}
	var flags uint64
	if raw := items[ItemFlags]; raw != "" {
		f, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, 0, err
		}
		flags = f
	}
	return value, flags, nil
}
