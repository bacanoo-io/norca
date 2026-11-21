// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package targetvalue

import (
	"errors"
	"fmt"
	"math"
	"strconv"
)

// ValidateConfig checks the parameters of a target-value check. It is used to
// validate builtin policies at submission, and by the strategy when it runs,
// so a policy stored before cannot reverse the scaling either.
//
// A value that would turn the scaling around is refused (review finding
// ASP-9): a negative or not finite target or threshold, and a negative
// max_scale_up or max_scale_down (a scale up capped to a count below the one
// the group has is a scale down).
func ValidateConfig(config map[string]string) error {
	t := config[runConfigKeyTarget]
	if t == "" {
		return errors.New("missing required field `target`")
	}
	target, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return fmt.Errorf("invalid value for `target`: %q is not a number", t)
	}
	if math.IsNaN(target) || math.IsInf(target, 0) || target < 0 {
		return fmt.Errorf("invalid value for `target`: %q must be a finite number of 0 or more", t)
	}
	if th := config[runConfigKeyThreshold]; th != "" {
		threshold, err := strconv.ParseFloat(th, 64)
		if err != nil {
			return fmt.Errorf("invalid value for `threshold`: %q is not a number", th)
		}
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 {
			return fmt.Errorf("invalid value for `threshold`: %q must be a finite number of 0 or more", th)
		}
	}
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
