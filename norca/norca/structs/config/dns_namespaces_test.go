// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
)

// The namespaces of the dns block may be patterns after the first, which is
// the namespace of names without one (review finding PLD-3).
func TestDNSConfig_ValidateNamespaces(t *testing.T) {
	ci.Parallel(t)

	for _, ok := range [][]string{{"default"}, {"default", "team-*"}, {"default", "*"}, {"apps", "team-[ab]"}} {
		must.NoError(t, (&DNSConfig{Namespaces: ok}).Validate(), must.Sprint(ok))
	}
	cases := []struct {
		namespaces []string
		err        string
	}{
		{[]string{"*"}, "the first of dns namespaces"},
		{[]string{"team-*", "default"}, "the first of dns namespaces"},
		{[]string{"default", "[bad"}, "not a valid pattern"},
		{[]string{"default", ""}, "must not be empty"},
	}
	for _, tc := range cases {
		must.ErrorContains(t, (&DNSConfig{Namespaces: tc.namespaces}).Validate(), tc.err, must.Sprint(tc.namespaces))
	}
}
