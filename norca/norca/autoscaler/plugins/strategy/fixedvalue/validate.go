// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package fixedvalue

import (
	"fmt"
	"strconv"
)

// ValidateConfig checks the parameters of a fixed-value check, as they are
// checked when the strategy runs. It is used to validate builtin policies at
// submission.
func ValidateConfig(config map[string]string) error {
	v := config[runConfigKeyValue]
	if v == "" {
		return fmt.Errorf("missing required field `%s`", runConfigKeyValue)
	}
	if _, err := strconv.ParseInt(v, 10, 64); err != nil {
		return fmt.Errorf("invalid value for `%s`: %q is not an integer", runConfigKeyValue, v)
	}
	return nil
}
