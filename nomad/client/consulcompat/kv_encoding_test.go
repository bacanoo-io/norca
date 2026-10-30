// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"regexp"
	"strings"
	"testing"

	"github.com/shoenig/test/must"
)

var kvTestKeys = []string{
	"a", "app/config", "app/db.url", "with space", "dir/", "a//b", "/lead", "tilde~x",
	"unicode/árvíztűrő", "=eq", "dots.and.more", "a/~", "trail//",
}

func TestKVKeyEncoding_Variables(t *testing.T) {
	path := regexp.MustCompile(`^[a-zA-Z0-9-_~/]+$`)
	for _, key := range kvTestKeys {
		enc := encodeKVKey(key, false)
		must.True(t, path.MatchString(enc), must.Sprintf("%q -> %q", key, enc))
		dec, err := decodeKVKey(enc)
		must.NoError(t, err)
		must.Eq(t, key, dec)
	}

	// the encoded prefix of a key's prefix is a prefix of the encoded key
	for _, key := range kvTestKeys {
		for i := 0; i <= len(key); i++ {
			p := key[:i]
			must.True(t, strings.HasPrefix(encodeKVKey(key, false), encodeKVKey(p, true)),
				must.Sprintf("prefix %q of %q", p, key))
		}
	}
	// and a key that does not start with the prefix does not match
	must.False(t, strings.HasPrefix(encodeKVKey("a/", false), encodeKVKey("a/~", true)))
	must.False(t, strings.HasPrefix(encodeKVKey("ab", false), encodeKVKey("a/", true)))
}

func TestKVKeyEncoding_NATS(t *testing.T) {
	// the key rules of the NATS key-value store
	natsKey := regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)
	for _, key := range kvTestKeys {
		enc := encodeNATSKey(key)
		must.True(t, natsKey.MatchString(enc), must.Sprintf("%q -> %q", key, enc))
		must.False(t, strings.HasPrefix(enc, ".") || strings.HasSuffix(enc, ".") || strings.Contains(enc, ".."), must.Sprint(enc))
		dec, err := decodeNATSKey(enc)
		must.NoError(t, err)
		must.Eq(t, key, dec)
	}
	must.Eq(t, ">", natsFilter("app", true))
	must.Eq(t, "app.>", natsFilter("app/", true))
	must.Eq(t, "app.sub.>", natsFilter("app/sub/x", true))
	must.Eq(t, "app.db=2Eurl", natsFilter("app/db.url", false))
}
