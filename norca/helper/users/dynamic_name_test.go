// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package users

import (
	"testing"

	"github.com/shoenig/test/must"
)

// TestIsDynamicName checks that IsDynamicName is true for exactly the names
// Lookup turns into an ID without looking them up.
func TestIsDynamicName(t *testing.T) {
	for name, dynamic := range map[string]bool{
		"norca:85000": true,
		"norca:1":     true,
		"norca:0":     false,
		"norca:-1":    false,
		"norca:":      false,
		"norca:staff": false,
		"norca":       false,
		"nobody":      false,
		"":            false,
		"1000:1000":   false,
	} {
		must.Eq(t, dynamic, IsDynamicName(name), must.Sprint(name))
		_, ok := dynamicUser(name)
		must.Eq(t, dynamic, ok, must.Sprint(name))
	}
}
