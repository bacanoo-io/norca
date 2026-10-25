// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package targetvalue

import (
	"errors"
	"fmt"
	"strconv"
)

// ValidateConfig checks the parameters of a target-value check, as they are
// checked when the strategy runs. It is used to validate builtin policies at
// submission.
func ValidateConfig(config map[string]string) error {
	t := config[runConfigKeyTarget]
	if t == "" {
		return errors.New("missing required field `target`")
	}
	if _, err := strconv.ParseFloat(t, 64); err != nil {
		return fmt.Errorf("invalid value for `target`: %q is not a number", t)
	}
	if th := config[runConfigKeyThreshold]; th != "" {
		if _, err := strconv.ParseFloat(th, 64); err != nil {
			return fmt.Errorf("invalid value for `threshold`: %q is not a number", th)
		}
	}
	for _, k := range []string{runConfigKeyMaxScaleUp, runConfigKeyMaxScaleDown} {
		if v := config[k]; v != "" {
			if _, err := strconv.ParseInt(v, 10, 64); err != nil {
				return fmt.Errorf("invalid value for `%s`: %q is not an integer", k, v)
			}
		}
	}
	return nil
}
