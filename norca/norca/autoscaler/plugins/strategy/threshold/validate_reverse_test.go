// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package threshold

import (
	"testing"

	"github.com/shoenig/test/must"
)

// Settings that make a threshold check fire wrongly or never are refused
// (review finding ASP-9).
func TestValidateConfig_ReversingValues(t *testing.T) {
	for _, cfg := range []map[string]string{
		{"lower_bound": "10", "delta": "1", "within_bounds_trigger": "0"},
		{"lower_bound": "10", "delta": "1", "within_bounds_trigger": "-3"},
		{"lower_bound": "10", "percentage": "NaN"},
		{"lower_bound": "10", "percentage": "+Inf"},
		{"lower_bound": "100", "upper_bound": "50", "delta": "1"},
		{"lower_bound": "NaN", "delta": "1"},
	} {
		must.Error(t, ValidateConfig(cfg), must.Sprintf("%v", cfg))
	}
	must.NoError(t, ValidateConfig(map[string]string{"lower_bound": "10", "upper_bound": "50", "percentage": "-50"}))
}
