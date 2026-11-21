// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package passthrough

import (
	"testing"

	"github.com/shoenig/test/must"
)

// The settings of pass-through are checked at admission like those of the
// other built-in strategies (review finding ASP-9).
func TestValidateConfig(t *testing.T) {
	must.NoError(t, ValidateConfig(map[string]string{}))
	must.NoError(t, ValidateConfig(map[string]string{"max_scale_up": "2", "max_scale_down": "0"}))
	must.Error(t, ValidateConfig(map[string]string{"max_scale_up": "x"}))
	must.Error(t, ValidateConfig(map[string]string{"max_scale_down": "-1"}))
}
