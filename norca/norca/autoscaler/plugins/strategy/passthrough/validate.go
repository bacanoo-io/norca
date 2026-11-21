// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package passthrough

import (
	"fmt"
	"strconv"
)

// ValidateConfig checks the parameters of a pass-through check. It is used to
// validate builtin policies at submission, and by the strategy when it runs:
// a max_scale_up or max_scale_down that is not an integer failed every
// evaluation, and a negative one turned the scaling around (review finding
// ASP-9).
func ValidateConfig(config map[string]string) error {
	for _, k := range []string{runConfigKeyMaxScaleUp, runConfigKeyMaxScaleDown} {
		if v := config[k]; v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid value for `%s`: %q is not an integer", k, v)
			}
			if n < 0 {
				return fmt.Errorf("invalid value for `%s`: %q is negative", k, v)
			}
		}
	}
	return nil
}
